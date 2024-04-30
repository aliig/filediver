package app

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gobwas/glob"
	"github.com/xypwn/filediver/exec"
	"github.com/xypwn/filediver/extractor"
	extr_bik "github.com/xypwn/filediver/extractor/bik"
	extr_texture "github.com/xypwn/filediver/extractor/texture"
	extr_unit "github.com/xypwn/filediver/extractor/unit"
	extr_wwise "github.com/xypwn/filediver/extractor/wwise"
	"github.com/xypwn/filediver/steampath"
	"github.com/xypwn/filediver/stingray"
)

var ConfigFormat = ConfigTemplate{
	Extractors: map[string]ConfigTemplateExtractor{
		"wwise_stream": {
			Category: "audio",
			Options: map[string]ConfigTemplateOption{
				"format": {
					PossibleValues: []string{"ogg", "wav", "aac", "mp3", "wem", "source"},
				},
			},
		},
		"wwise_bank": {
			Category: "audio",
			Options: map[string]ConfigTemplateOption{
				"format": {
					PossibleValues: []string{"ogg", "wav", "aac", "mp3", "bnk", "source"},
				},
			},
		},
		"bik": {
			Category: "video",
			Options: map[string]ConfigTemplateOption{
				"format": {
					PossibleValues: []string{"mp4", "bik", "source"},
				},
			},
		},
		"texture": {
			Category: "image",
			Options: map[string]ConfigTemplateOption{
				"format": {
					PossibleValues: []string{"png", "dds", "source"},
				},
			},
		},
		"unit": {
			Category: "model",
			Options: map[string]ConfigTemplateOption{
				"format": {
					PossibleValues: []string{"glb", "source"},
				},
				"meshes": {
					PossibleValues: []string{"highest_detail", "all"},
				},
			},
		},
		"raw": {
			Category: "",
			Options: map[string]ConfigTemplateOption{
				"format": {
					PossibleValues: []string{"source"},
				},
			},
			DefaultDisabled: true,
		},
	},
	Fallback: "raw",
}

func parseWwiseDep(ctx context.Context, f *stingray.File) (string, error) {
	r, err := f.Open(ctx, stingray.DataMain)
	if err != nil {
		return "", err
	}
	var magicNum [4]byte
	if _, err := io.ReadFull(r, magicNum[:]); err != nil {
		return "", err
	}
	if magicNum != [4]byte{0xd8, '/', 'v', 'x'} {
		return "", errors.New("invalid magic number")
	}
	var textLen uint32
	if err := binary.Read(r, binary.LittleEndian, &textLen); err != nil {
		return "", err
	}
	text := make([]byte, textLen-1)
	if _, err := io.ReadFull(r, text); err != nil {
		return "", err
	}
	return string(text), nil
}

// Returns error if steam path couldn't be found.
func DetectGameDir() (string, error) {
	return steampath.GetAppPath("553850", "Helldivers 2")
}

func VerifyGameDir(path string) error {
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return fmt.Errorf("invalid game directory: %v: not a directory", path)
	}
	if info, err := os.Stat(filepath.Join(path, "settings.ini")); err == nil && info.Mode().IsRegular() {
		// We were given the "data" directory => go back
		path = filepath.Dir(path)
	}
	if info, err := os.Stat(filepath.Join(path, "data", "settings.ini")); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("invalid game directory: %v: valid data directory not found", path)
	}
	return nil
}

func ParseHashes(str string) []string {
	var res []string
	sc := bufio.NewScanner(strings.NewReader(str))
	for sc.Scan() {
		s := strings.TrimSpace(sc.Text())
		if s != "" && !strings.HasPrefix(s, "//") {
			res = append(res, s)
		}
	}
	return res
}

type App struct {
	Hashes  map[stingray.Hash]string
	DataDir *stingray.DataDir
}

// Open game dir and read metadata.
func OpenGameDir(ctx context.Context, gameDir string, hashes []string, onProgress func(curr, total int)) (*App, error) {
	dataDir, err := stingray.OpenDataDir(ctx, filepath.Join(gameDir, "data"), onProgress)
	if err != nil {
		return nil, err
	}

	hashesMap := make(map[stingray.Hash]string)
	for _, h := range hashes {
		hashesMap[stingray.Sum64([]byte(h))] = h
	}
	// wwise_dep files let us know the string of many of the wwise_banks
	for id, file := range dataDir.Files {
		if id.Type == stingray.Sum64([]byte("wwise_dep")) {
			h, err := parseWwiseDep(ctx, file)
			if err != nil {
				return nil, fmt.Errorf("wwise_dep: %w", err)
			}
			hashesMap[stingray.Sum64([]byte(h))] = h
		}
	}

	return &App{
		Hashes:  hashesMap,
		DataDir: dataDir,
	}, nil
}

func (a *App) matchFileID(id stingray.FileID, glb glob.Glob, nameOnly bool) bool {
	nameVariations := []string{
		id.Name.StringEndian(binary.LittleEndian),
		id.Name.StringEndian(binary.BigEndian),
		"0x" + id.Name.StringEndian(binary.BigEndian),
		"0x" + strings.TrimLeft(id.Name.StringEndian(binary.BigEndian), "0"),
	}
	if name, ok := a.Hashes[id.Name]; ok {
		nameVariations = append(nameVariations, name)
	}

	var typeVariations []string
	if !nameOnly {
		typeVariations = []string{
			id.Type.StringEndian(binary.LittleEndian),
			id.Type.StringEndian(binary.BigEndian),
			"0x" + id.Type.StringEndian(binary.BigEndian),
			"0x" + strings.TrimLeft(id.Type.StringEndian(binary.BigEndian), "0"),
		}
		if typ, ok := a.Hashes[id.Type]; ok {
			typeVariations = append(typeVariations, typ)
		}
	}

	for _, name := range nameVariations {
		if nameOnly {
			if glb.Match(name) {
				return true
			}
		} else {
			for _, typ := range typeVariations {
				if glb.Match(name + "." + typ) {
					return true
				}
			}
		}
	}

	return false
}

func (a *App) MatchingFiles(includeGlob, excludeGlob string, cfgTemplate ConfigTemplate, cfg map[string]map[string]string) (map[stingray.FileID]*stingray.File, error) {
	var inclGlob glob.Glob
	inclGlobNameOnly := !strings.Contains(includeGlob, ".")
	if includeGlob != "" {
		var err error
		inclGlob, err = glob.Compile(includeGlob)
		if err != nil {
			return nil, err
		}
	}
	var exclGlob glob.Glob
	exclGlobNameOnly := !strings.Contains(excludeGlob, ".")
	if excludeGlob != "" {
		var err error
		exclGlob, err = glob.Compile(excludeGlob)
		if err != nil {
			return nil, err
		}
	}

	res := make(map[stingray.FileID]*stingray.File)
	for id, file := range a.DataDir.Files {
		shouldIncl := true
		if includeGlob != "" {
			shouldIncl = a.matchFileID(id, inclGlob, inclGlobNameOnly)
		}
		if excludeGlob != "" {
			if a.matchFileID(id, exclGlob, exclGlobNameOnly) {
				shouldIncl = false
			}
		}
		if !shouldIncl {
			continue
		}

		typ, ok := a.Hashes[id.Type]
		if !ok {
			typ = id.Type.String()
		}
		_, knownType := cfgTemplate.Extractors[typ]
		if !knownType {
			typ = cfgTemplate.Fallback
		}

		shouldExtract := true
		if extrTempl, ok := cfgTemplate.Extractors[typ]; ok {
			shouldExtract = !extrTempl.DefaultDisabled
		}
		if cfg["enable"] != nil {
			shouldExtract = cfg["enable"][typ] == "true"
		}
		if cfg["disable"] != nil {
			if cfg["disable"][typ] == "true" {
				shouldExtract = false
			}
		}
		if !shouldExtract {
			continue
		}

		res[id] = file
	}

	return res, nil
}

type extractContext struct {
	ctx     context.Context
	app     *App
	file    *stingray.File
	runner  *exec.Runner
	config  map[string]string
	outPath string
	files   []string
}

func newExtractContext(ctx context.Context, app *App, file *stingray.File, runner *exec.Runner, config map[string]string, outPath string) *extractContext {
	return &extractContext{
		ctx:     ctx,
		app:     app,
		file:    file,
		runner:  runner,
		config:  config,
		outPath: outPath,
	}
}

func (c *extractContext) OutPath() (string, error)  { return c.outPath, nil }
func (c *extractContext) File() *stingray.File      { return c.file }
func (c *extractContext) Runner() *exec.Runner      { return c.runner }
func (c *extractContext) Config() map[string]string { return c.config }
func (c *extractContext) GetResource(name, typ stingray.Hash) (file *stingray.File, exists bool) {
	file, exists = c.app.DataDir.Files[stingray.FileID{Name: name, Type: typ}]
	return
}
func (c *extractContext) CreateFile(suffix string) (io.WriteCloser, error) {
	path, err := c.AllocateFile(suffix)
	if err != nil {
		return nil, err
	}
	return os.Create(path)
}
func (c *extractContext) AllocateFile(suffix string) (string, error) {
	path := c.outPath + suffix
	if err := os.MkdirAll(filepath.Dir(path), os.ModePerm); err != nil {
		return "", err
	}
	c.files = append(c.files, path)
	return path, nil
}
func (c *extractContext) Ctx() context.Context { return c.ctx }
func (c *extractContext) Files() []string {
	return c.files
}

// Returns path to extracted file/directory.
func (a *App) ExtractFile(ctx context.Context, id stingray.FileID, outDir string, extrCfg map[string]map[string]string, runner *exec.Runner) ([]string, error) {
	name, ok := a.Hashes[id.Name]
	if !ok {
		name = id.Name.String()
	}
	typ, ok := a.Hashes[id.Type]
	if !ok {
		typ = id.Type.String()
	}

	file, ok := a.DataDir.Files[id]
	if !ok {
		return nil, fmt.Errorf("extract %v.%v: file does not exist", name, typ)
	}

	cfg := extrCfg[typ]
	if cfg == nil {
		cfg = make(map[string]string)
	}

	var extr extractor.ExtractFunc
	switch typ {
	case "bik":
		if cfg["format"] == "source" {
			extr = extractor.ExtractFuncRaw(".stingray_bik", stingray.DataMain, stingray.DataStream, stingray.DataGPU)
		} else if cfg["format"] == "bik" {
			extr = extr_bik.ExtractBik
		} else {
			extr = extr_bik.ConvertToMP4
		}
	case "wwise_stream":
		if cfg["format"] == "source" {
			extr = extractor.ExtractFuncRaw(".stingray_wem", stingray.DataMain, stingray.DataStream, stingray.DataGPU)
		} else if cfg["format"] == "wem" {
			extr = extractor.ExtractFuncRaw(".wem", stingray.DataStream)
		} else {
			extr = extr_wwise.ConvertWem
		}
	case "wwise_bank":
		if cfg["format"] == "source" {
			extr = extractor.ExtractFuncRaw(".bnk", stingray.DataMain, stingray.DataStream, stingray.DataGPU)
		} else if cfg["format"] == "bnk" {
			extr = extr_wwise.ExtractBnk
		} else {
			extr = extr_wwise.ConvertBnk
		}
	case "unit":
		if cfg["format"] == "source" {
			extr = extractor.ExtractFuncRaw(".unit", stingray.DataMain, stingray.DataStream, stingray.DataGPU)
		} else {
			extr = extr_unit.Convert
		}
	case "texture":
		if cfg["format"] == "source" {
			extr = extractor.ExtractFuncRaw(".texture", stingray.DataMain, stingray.DataStream, stingray.DataGPU)
		} else if cfg["format"] == "dds" {
			extr = extr_texture.ExtractDDS
		} else {
			extr = extr_texture.ConvertToPNG
		}
	default:
		extr = extractor.ExtractFuncRaw(typ)
	}

	outPath := filepath.Join(outDir, name)
	if err := os.MkdirAll(filepath.Dir(outPath), os.ModePerm); err != nil {
		return nil, err
	}
	extrCtx := newExtractContext(
		ctx,
		a,
		file,
		runner,
		cfg,
		outPath,
	)
	if err := extr(extrCtx); err != nil {
		{
			var err error
			var errPath string
			for _, path := range extrCtx.Files() {
				if e := os.Remove(path); e != nil && !errors.Is(e, os.ErrNotExist) && err == nil {
					err = e
					errPath = path
				}
			}
			if err != nil {
				return nil, fmt.Errorf("cleanup %v: %w", errPath, err)
			}
		}
		return nil, fmt.Errorf("extract %v.%v: %w", name, typ, err)
	}

	return extrCtx.Files(), nil
}
