package unit

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"strconv"

	"github.com/qmuntal/gltf"
	"github.com/qmuntal/gltf/modeler"

	"github.com/go-gl/mathgl/mgl32"

	"github.com/xypwn/filediver/extractor"
	"github.com/xypwn/filediver/stingray"
	"github.com/xypwn/filediver/stingray/bones"
	"github.com/xypwn/filediver/stingray/unit"
	"github.com/xypwn/filediver/stingray/unit/material"
	"github.com/xypwn/filediver/stingray/unit/texture"
)

type ImageOptions struct {
	Jpeg           bool                 // PNG if false, JPEG if true
	JpegQuality    int                  // Quality if Jpeg == true; interval = [1;100]; 0 for default quality
	PngCompression png.CompressionLevel // Compression if Jpeg == false
}

// Adds back in the truncated Z component of a normal map.
func postProcessReconstructNormalZ(img image.Image) error {
	calcZ := func(x, y float64) float64 {
		return math.Sqrt(-x*x - y*y + 1)
	}
	switch img := img.(type) {
	case *image.NRGBA:
		for iY := img.Rect.Min.Y; iY < img.Rect.Max.Y; iY++ {
			for iX := img.Rect.Min.X; iX < img.Rect.Max.X; iX++ {
				idx := img.PixOffset(iX, iY)
				r, g := img.Pix[idx], img.Pix[idx+1]
				x, y := (float64(r)/127.5)-1, (float64(g)/127.5)-1
				z := calcZ(x, y)
				img.Pix[idx+2] = uint8(math.Round((z + 1) * 127.5))
			}
		}
		return nil
	default:
		return errors.New("postProcessReconstructNormalZ: unsupported image type")
	}
}

// Attempts to completely remove the influence of the alpha channel,
// giving the whole image an opacity of 1.
func postProcessToOpaque(img image.Image) error {
	switch img := img.(type) {
	case *image.NRGBA:
		for iY := img.Rect.Min.Y; iY < img.Rect.Max.Y; iY++ {
			for iX := img.Rect.Min.X; iX < img.Rect.Max.X; iX++ {
				idx := img.PixOffset(iX, iY)
				img.Pix[idx+3] = 255
			}
		}
		return nil
	default:
		return errors.New("postProcessToOpaque: unsupported image type")
	}
}

type textureType int

const (
	textureTypeBaseColor textureType = iota
	textureTypeNormal
)

func tryWriteTexture(ctx extractor.Context, mat *material.Material, texType textureType, doc *gltf.Document, imgOpts *ImageOptions) (uint32, bool, error) {
	var id stingray.Hash
	var postProcess func(image.Image) error
	switch texType {
	case textureTypeBaseColor:
		var ok bool
		id, ok = mat.Textures[stingray.Sum64([]byte("albedo_iridescence")).Thin()]
		if ok {
			postProcess = postProcessToOpaque
			break
		}
		id, ok = mat.Textures[stingray.Sum64([]byte("albedo")).Thin()]
		if ok {
			break
		}
		return 0, false, nil
	case textureTypeNormal:
		var ok bool
		id, ok = mat.Textures[stingray.Sum64([]byte("normal")).Thin()]
		if ok {
			postProcess = postProcessReconstructNormalZ
			break
		}
		return 0, false, nil
	default:
		panic("unhandled case")
	}
	res, err := writeTexture(ctx, doc, id, postProcess, imgOpts)
	if err != nil {
		return 0, false, err
	}
	return res, true, nil
}

// Adds a texture to doc. Returns new texture ID if err != nil.
// postProcess optionally applies image post-processing.
func writeTexture(ctx extractor.Context, doc *gltf.Document, id stingray.Hash, postProcess func(image.Image) error, imgOpts *ImageOptions) (uint32, error) {
	file, exists := ctx.GetResource(id, stingray.Sum64([]byte("texture")))
	if !exists || !file.Exists(stingray.DataMain) {
		return 0, fmt.Errorf("texture resource %v doesn't exist", id)
	}

	tex, err := texture.Decode(ctx.Ctx(), file, false)
	if err != nil {
		return 0, err
	}

	if postProcess != nil {
		if err := postProcess(tex.Image); err != nil {
			return 0, err
		}
	}
	var encData bytes.Buffer
	var mimeType string
	if imgOpts != nil && imgOpts.Jpeg {
		quality := jpeg.DefaultQuality
		if imgOpts.JpegQuality != 0 {
			quality = imgOpts.JpegQuality
		}
		if err := jpeg.Encode(&encData, tex, &jpeg.Options{Quality: quality}); err != nil {
			return 0, err
		}
		mimeType = "image/jpeg"
	} else {
		compression := png.DefaultCompression
		if imgOpts != nil {
			compression = imgOpts.PngCompression
		}
		if err := (&png.Encoder{
			CompressionLevel: compression,
		}).Encode(&encData, tex); err != nil {
			return 0, err
		}
		mimeType = "image/png"
	}
	imgIdx, err := modeler.WriteImage(doc, id.String(), mimeType, &encData)
	if err != nil {
		return 0, err
	}
	doc.Textures = append(doc.Textures, &gltf.Texture{
		Sampler: gltf.Index(0),
		Source:  gltf.Index(imgIdx),
	})
	return uint32(len(doc.Textures) - 1), nil
}

func loadBoneMap(ctx extractor.Context) (*bones.BoneInfo, error) {
	bonesId := ctx.File().ID()
	bonesId.Type = stingray.Sum64([]byte("bones"))
	bonesFile, exists := ctx.GetResource(bonesId.Name, bonesId.Type)
	if !exists {
		return nil, fmt.Errorf("loadBoneMap: bones file does not exist")
	}
	bonesMain, err := bonesFile.Open(ctx.Ctx(), stingray.DataMain)
	if err != nil {
		return nil, fmt.Errorf("loadBoneMap: bones file does not have a main component")
	}

	boneInfo, err := bones.LoadBones(bonesMain)
	return boneInfo, err
}

// Adds the unit's skeleton to the gltf document
func addSkeleton(doc *gltf.Document, unitInfo *unit.Info, boneInfo *bones.BoneInfo) uint32 {
	var matrices [][4][4]float32 = make([][4][4]float32, len(unitInfo.JointTransformMatrices))
	gltfConversionMatrix := mgl32.HomogRotate3DX(mgl32.DegToRad(-90.0)).Mul4(mgl32.HomogRotate3DZ(mgl32.DegToRad(-90.0)))
	for i := range matrices {
		jtm := unitInfo.JointTransformMatrices[i]
		bindMatrix := mgl32.Mat4FromRows(jtm[0], jtm[1], jtm[2], jtm[3]).Transpose()
		bindMatrix = gltfConversionMatrix.Mul4(bindMatrix)
		row0, row1, row2, row3 := bindMatrix.Inv().Rows()
		matrices[i] = [4][4]float32{row0, row1, row2, row3}
	}

	for i, index := range unitInfo.SkeletonMaps[0].BoneIndices {
		skm := unitInfo.SkeletonMaps[0].Matrices[i]
		bindMatrix := mgl32.Mat4FromRows(skm[0], skm[1], skm[2], skm[3]).Transpose().Inv()
		bindMatrix = gltfConversionMatrix.Mul4(bindMatrix)
		row0, row1, row2, row3 := bindMatrix.Inv().Rows()
		matrices[index] = [4][4]float32{row0, row1, row2, row3}
	}

	inverseBindMatrices := modeler.WriteAccessor(doc, gltf.TargetArrayBuffer, matrices)
	jointIndices := make([]uint32, 0)
	boneBaseIndex := uint32(len(doc.Nodes))
	for i, bone := range unitInfo.Bones {
		var rot [3][3]float32 = bone.Transform.Rotation
		quat := mgl32.Mat4ToQuat(mgl32.Mat3FromRows(rot[0], rot[1], rot[2]).Mat4())
		t := bone.Transform.Translation
		t[0], t[1], t[2] = t[1], t[2], t[0]
		s := bone.Transform.Scale
		//s[0], s[1], s[2] = s[1], s[2], s[0]
		boneName := fmt.Sprintf("%d:Bone_%08X", i, bone.NameHash.Value)
		if boneInfo != nil {
			name, exists := boneInfo.NameMap[bone.NameHash]
			if exists {
				boneName = fmt.Sprintf("%d:%s", i, name)
			}
		}
		doc.Nodes = append(doc.Nodes, &gltf.Node{
			Name:        boneName,
			Rotation:    [4]float32{quat.X(), quat.Y(), quat.Z(), quat.W},
			Translation: t,
			Scale:       s,
		})
		boneIdx := uint32(len(doc.Nodes) - 1)
		jointIndices = append(jointIndices, boneIdx)
		parentIndex := bone.ParentIndex + boneBaseIndex
		if parentIndex != boneIdx {
			doc.Nodes[parentIndex].Children = append(doc.Nodes[parentIndex].Children, boneIdx)
		}
	}

	doc.Skins = append(doc.Skins, &gltf.Skin{
		InverseBindMatrices: gltf.Index(inverseBindMatrices),
		Joints:              jointIndices,
	})

	return uint32(len(doc.Skins) - 1)
}

func ConvertOpts(ctx extractor.Context, imgOpts *ImageOptions) error {
	fMain, err := ctx.File().Open(ctx.Ctx(), stingray.DataMain)
	if err != nil {
		return err
	}
	defer fMain.Close()
	var fGPU io.ReadSeekCloser
	if ctx.File().Exists(stingray.DataGPU) {
		fGPU, err = ctx.File().Open(ctx.Ctx(), stingray.DataGPU)
		if err != nil {
			return err
		}
		defer fGPU.Close()
	}

	boneInfo, _ := loadBoneMap(ctx)

	unitInfo, err := unit.LoadInfo(fMain)
	if err != nil {
		return err
	}

	doc := gltf.NewDocument()
	doc.Asset.Generator = "https://github.com/xypwn/filediver"
	doc.Samplers = append(doc.Samplers, &gltf.Sampler{
		MagFilter: gltf.MagLinear,
		MinFilter: gltf.MinLinear,
		WrapS:     gltf.WrapRepeat,
		WrapT:     gltf.WrapRepeat,
	})

	// Load materials
	materialIdxs := make(map[stingray.ThinHash]uint32)
	for id, resID := range unitInfo.Materials {
		matRes, exists := ctx.GetResource(resID, stingray.Sum64([]byte("material")))
		if !exists || !matRes.Exists(stingray.DataMain) {
			return fmt.Errorf("referenced material resource %v doesn't exist", resID)
		}
		mat, err := func() (*material.Material, error) {
			f, err := matRes.Open(ctx.Ctx(), stingray.DataMain)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			return material.Load(f)
		}()
		if err != nil {
			return err
		}

		/*materialNames := make(map[stingray.ThinHash]string)
		f, err := os.Open("material_textures.txt")
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			s := sc.Text()
			materialNames[stingray.Sum64([]byte(s)).Thin()] = s
		}
		fmt.Println()
		for k, v := range mat.Textures {
			name := k.String()
			if s, ok := materialNames[k]; ok {
				name = s
			}
			fmt.Println(name, v)
		}*/

		/*for k, v := range mat.Textures {
			texRes, exists := ctx.GetResource(v, stingray.Sum64([]byte("texture")))
			if !exists || !texRes.Exists(stingray.DataMain) {
				return fmt.Errorf("texture resource %v doesn't exist", id)
			}

			tex, err := texture.Decode(texRes, false)
			if err != nil {
				return err
			}

			if err := func() error {
				out, err := ctx.CreateFileDir(".unit.textures", k.String()+"_"+v.String()+".png")
				if err != nil {
					return err
				}
				defer out.Close()
				if err := png.Encode(out, tex); err != nil {
					return err
				}
				return nil
			}(); err != nil {
				return err
			}
		}*/

		texIdxBaseColor, ok, err := tryWriteTexture(ctx, mat, textureTypeBaseColor, doc, imgOpts)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		texIdxNormal, ok, err := tryWriteTexture(ctx, mat, textureTypeNormal, doc, imgOpts)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		doc.Materials = append(doc.Materials, &gltf.Material{
			Name: resID.String(),
			PBRMetallicRoughness: &gltf.PBRMetallicRoughness{
				BaseColorTexture: &gltf.TextureInfo{
					Index: texIdxBaseColor,
				},
				MetallicFactor:  gltf.Float(0.5),
				RoughnessFactor: gltf.Float(1),
			},
			NormalTexture: &gltf.NormalTexture{
				Index: gltf.Index(texIdxNormal),
			},
		})
		materialIdxs[id] = uint32(len(doc.Materials) - 1)
	}

	// Determine which meshes to convert
	var meshesToLoad []uint32
	switch ctx.Config()["meshes"] {
	case "all":
		for i := uint32(0); i < unitInfo.NumMeshes; i++ {
			meshesToLoad = append(meshesToLoad, i)
		}
	default: // "highest_detail"
		if len(unitInfo.LODGroups) > 0 {
			entries := unitInfo.LODGroups[0].Entries
			highestDetailIdx := -1
			for i := range entries {
				if highestDetailIdx == -1 || entries[i].Detail.Max > entries[highestDetailIdx].Detail.Max {
					highestDetailIdx = i
				}
			}
			if highestDetailIdx != -1 {
				meshesToLoad = entries[highestDetailIdx].Indices
			}
		}
	}

	// Load meshes
	meshes, err := unit.LoadMeshes(fGPU, unitInfo, meshesToLoad)
	if err != nil {
		return err
	}
	for _, meshID := range meshesToLoad {
		if meshID >= unitInfo.NumMeshes {
			panic("meshID out of bounds")
		}

		mesh := meshes[meshID]
		if len(mesh.UVCoords) == 0 {
			continue
		}

		// Transform coordinates into glTF ones
		for i := range mesh.Positions {
			p := mesh.Positions[i]
			p[0], p[1], p[2] = p[1], p[2], p[0]
			mesh.Positions[i] = p
		}

		// Components of the model (damage states, separate parts, etc) seem to be distinguished by their
		// UV coordinates. The range appears to be [0, 32), so theoretically there could be
		// 1024 components in a mesh.
		//    - a charger's intact head has UV coords of (8.x, 0.x), while the destroyed head
		//      has UV coords of (9.x, 0.x)
		//    - a bile titan's undamaged front left leg has UV coords of (31.X, 0.X), and its damaged
		//      front left leg has UV coords of (0.x, 1.x)
		var components map[uint32][]uint32 = make(map[uint32][]uint32)
		componentCfg := ctx.Config()["components"]
		if componentCfg == "split" {
			for i := range mesh.Indices {
				uv := mesh.UVCoords[mesh.Indices[i]]
				key := uint32(uv[0]) + (uint32(uv[1]) << 5)
				if uv[1] < 0 {
					key = uint32(uv[0]) + (uint32((-uv[1])+1) << 5)
				}
				components[key] = append(components[key], mesh.Indices[i])
			}
		} else {
			key := uint32(0)
			components[key] = append(components[key], mesh.Indices...)
		}

		var material *uint32
		if len(mesh.Info.Materials) > 0 {
			if idx, ok := materialIdxs[mesh.Info.Materials[0]]; ok {
				material = gltf.Index(idx)
			}
		}

		if len(unitInfo.SkeletonMaps) > 0 {
			for i := range mesh.BoneIndices {
				for j := range mesh.BoneIndices[i] {
					remapIndex := unitInfo.SkeletonMaps[0].RemapData.Indices[mesh.BoneIndices[i][j]]
					mesh.BoneIndices[i][j] = uint8(unitInfo.SkeletonMaps[0].BoneIndices[remapIndex])
				}
			}
		}

		skin := addSkeleton(doc, unitInfo, boneInfo)

		positions := modeler.WritePosition(doc, mesh.Positions)
		texCoords := modeler.WriteTextureCoord(doc, mesh.UVCoords)
		weights := modeler.WriteWeights(doc, mesh.BoneWeights)
		joints := modeler.WriteJoints(doc, mesh.BoneIndices)
		meshIndex := len(doc.Meshes)
		parentIndex := len(doc.Nodes)
		name := fmt.Sprintf("Mesh %v", meshIndex)
		parent := gltf.Node{
			Name: name,
		}
		doc.Nodes = append(doc.Nodes, &parent)
		for k := range components {
			cmpStr := ""
			if len(components) > 1 {
				cmpStr = fmt.Sprintf(" Component %d", k)
			}
			name := fmt.Sprintf("Mesh %d%s", meshIndex, cmpStr)
			doc.Meshes = append(doc.Meshes, &gltf.Mesh{
				Name: name,
				Primitives: []*gltf.Primitive{
					{
						Indices: gltf.Index(modeler.WriteIndices(doc, components[k])),
						Attributes: map[string]uint32{
							gltf.POSITION:   positions,
							gltf.TEXCOORD_0: texCoords,
							//gltf.COLOR_0:    modeler.WriteColor(doc, mesh.Colors),
							gltf.JOINTS_0:  joints,
							gltf.WEIGHTS_0: weights,
						},
						Material: material,
					},
				},
			})
			if len(components) > 1 {
				doc.Nodes = append(doc.Nodes, &gltf.Node{
					Name: name,
					Mesh: gltf.Index(uint32(len(doc.Meshes) - 1)),
					Skin: gltf.Index(skin),
				})
				parent.Children = append(parent.Children, uint32(len(doc.Nodes)-1))
			} else {
				parent.Mesh = gltf.Index(uint32(len(doc.Meshes) - 1))
				parent.Skin = gltf.Index(skin)
			}
		}

		doc.Scenes[0].Nodes = append(doc.Scenes[0].Nodes, uint32(parentIndex))
	}

	out, err := ctx.CreateFile(".glb")
	if err != nil {
		return err
	}
	enc := gltf.NewEncoder(out)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	return nil
}

func Convert(ctx extractor.Context) error {
	var opts ImageOptions
	if v, ok := ctx.Config()["image_jpeg"]; ok && v == "true" {
		opts.Jpeg = true
	}
	if v, ok := ctx.Config()["jpeg_quality"]; ok {
		quality, err := strconv.Atoi(v)
		if err != nil {
			return err
		}
		opts.JpegQuality = quality
	}
	if v, ok := ctx.Config()["png_compression"]; ok {
		switch v {
		case "default":
			opts.PngCompression = png.DefaultCompression
		case "none":
			opts.PngCompression = png.NoCompression
		case "fast":
			opts.PngCompression = png.BestSpeed
		case "best":
			opts.PngCompression = png.BestCompression
		}
	}
	return ConvertOpts(ctx, &opts)
}
