package main

import (
	"bufio"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"

	//"github.com/davecgh/go-spew/spew"

	"github.com/gobwas/glob"
	"github.com/hellflame/argparse"

	"github.com/xypwn/filediver/exec"
	"github.com/xypwn/filediver/extractor"
	extr_bik "github.com/xypwn/filediver/extractor/bik"
	extr_texture "github.com/xypwn/filediver/extractor/texture"
	extr_unit "github.com/xypwn/filediver/extractor/unit"
	extr_wwise "github.com/xypwn/filediver/extractor/wwise"
	"github.com/xypwn/filediver/steampath"
	"github.com/xypwn/filediver/stingray"
)

//go:embed files.txt
var knownFilesStr string

//go:embed types.txt
var knownTypesStr string

func extractStingrayFile(outDirPath string, file *stingray.File, name, typ string, cfg extractor.Config, runner *exec.Runner, getResource extractor.GetResourceFunc) (bool, error) {
	modeConvert := true
	if cfg, ok := cfg["conv"]; ok {
		if cfg != "true" && cfg != "false" {
			return false, fmt.Errorf("extractor config: \"%v:conv=\": expected true or false, but got: %v", typ, cfg)
		}
		modeConvert = cfg != "true"
	}

	var extr extractor.ExtractFunc
	switch typ {
	case "bik":
		if modeConvert {
			extr = extr_bik.Convert
		} else {
			extr = extr_bik.Extract
		}
	case "wwise_stream":
		if modeConvert {
			extr = extr_wwise.ConvertWem
		} else {
			extr = extr_wwise.ExtractWem
		}
	case "wwise_bank":
		if modeConvert {
			extr = extr_wwise.ConvertBnk
		} else {
			extr = extr_wwise.ExtractBnk
		}
	case "unit":
		extr = extr_unit.Convert
		if !modeConvert {
			return false, fmt.Errorf("cannot extract \"unit\" file without conversion")
		}
	case "texture":
		if modeConvert {
			extr = extr_texture.Convert
		} else {
			extr = extr_texture.Extract
		}
	default:
		return false, nil
	}

	var readers [3]io.ReadSeeker
	foundDataTypes := 0
	for dataType := stingray.DataType(0); dataType < stingray.NumDataType; dataType++ {
		if !file.Exists(dataType) {
			continue
		}
		r, err := file.Open(dataType)
		if err != nil {
			return false, err
		}
		defer r.Close()
		readers[dataType] = r
		foundDataTypes++
	}
	if foundDataTypes == 0 {
		return false, nil
	}
	outPath := filepath.Join(outDirPath, name)
	if err := os.MkdirAll(filepath.Dir(outPath), os.ModePerm); err != nil {
		return false, err
	}
	if err := extr(outPath, readers, cfg, runner, getResource); err != nil {
		return false, fmt.Errorf("extract %v (type %v): %w", name, typ, err)
	}

	return true, nil
}

var extractorConfigValidKeys = map[string]struct{}{
	"enable":       {},
	"disable":      {},
	"wwise_stream": {},
	"wwise_bank":   {},
	"bik":          {},
	"texture":      {},
	"unit":         {},
}
var extractorConfigShorthands = map[string][]string{
	"audio": {"wwise_stream", "wwise_bank"},
	"video": {"bik"},
	"model": {"unit"},
}

func extractorConfigSubstituteShorthandKeys[T any](cfg map[string]T) error {
	for k, v := range cfg {
		if shs, ok := extractorConfigShorthands[k]; ok {
			for _, sh := range shs {
				cfg[sh] = v
			}
			continue
		}
		if _, ok := extractorConfigValidKeys[k]; ok {
			continue
		}
		return fmt.Errorf("invalid key: \"%v\"", k)
	}
	return nil
}

func parseExtractorConfig(s string) (map[string]extractor.Config, error) {
	res := make(map[string]extractor.Config)
	if s == "" {
		return res, nil
	}
	sp := strings.Split(s, " ")
	for _, s := range sp {
		k, v, ok := strings.Cut(s, ":")
		if !ok {
			return nil, fmt.Errorf("extractor config: expected \":\" to separate key and options")
		}
		cfg := make(extractor.Config)
		opts := strings.Split(v, ",")
		for _, opt := range opts {
			k, v, ok := strings.Cut(opt, "=")
			if !ok {
				v = "true"
			}
			cfg[k] = v
		}
		res[k] = cfg
	}

	// Substitute shorthands and validate keys
	if err := extractorConfigSubstituteShorthandKeys(res); err != nil {
		return nil, fmt.Errorf("extractor config: %w", err)
	}
	for _, cfg := range []extractor.Config{res["enable"], res["disable"]} {
		for k, v := range cfg {
			if v != "true" && v != "false" {
				return nil, fmt.Errorf("extractor config: \"enable/disable:%v=\": expected true or false, but got: \"%v\"", k, v)
			}
			if err := extractorConfigSubstituteShorthandKeys(cfg); err != nil {
				return nil, fmt.Errorf("extractor config: %w", err)
			}
		}
	}
	return res, nil
}

func main() {
	prt := newPrinter()

	if false {
		f, err := os.Create("cpu.prof")
		if err != nil {
			prt.Fatalf("could not create CPU profile: %v", err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			prt.Fatalf("could not start CPU profile: %v", err)
		}
		defer pprof.StopCPUProfile()
	}

	parser := argparse.NewParser("filediver", "An unofficial Helldivers 2 game asset extractor.", &argparse.ParserConfig{
		EpiLog: `matching files:
  Syntax is Glob (meaning *, ** etc. are supported)
  Basic format being matched is: <file_path>.<file_type>
  where file_type is NOT the extension, but the DATA TYPE (e.g. wwise_stream, texture etc.).
  examples:
    "content/audio/**.wwise_stream"              extract all wwise_stream files in content/audio, or any subfolders
    "{**.bik,**.wwise_stream,**.wwise_bank}"     extract all video and audio files (though easier with extractor config)
    "content/audio/us/303183090.wwise_stream"    extract one particular audio file (NOTE that the extension here is NOT the final extracted extension, but rather the data type)

extractor config:
  basic format: filediver -c "<key1>:<opt1>,<opt2> <key2>:<opt1>,<opt2>"
  examples:
    filediver -c "enable:audio"                 only extract audio
    filediver -c "disable:audio,video"          exclude audio and video
    filediver -c "enable:bik bik:conv=false"    only extract bik files, but don't convert them to mp4
    filediver -c "audio:format=ogg"             convert audio to ogg instead of wav
  special keys:
    enable:<list>     enable only the specified extractors
    disable:<list>    enable all extractors, except the specified ones
  special format keys:
    audio      all audio formats
    video      all video formats
  format keys:
    bik             Bink video
    texture         texture
    wwise_stream    singular Wwise audio stream
    wwise_bank      container with multiple Wwise audio streams
  options:
    all:
      conv=true|false    if false, file will be copied in its original format (probably can't be opened by most programs), default: true
    audio, wwise_stream, wwise_bank:
      format=wav|ogg|aac|mp3    output format (anything other than WAV requires FFmpeg), default: ogg`,
		DisableDefaultShowHelp: true,
	})
	gameDir := parser.String("g", "gamedir", &argparse.Option{Help: "Helldivers 2 game directory"})
	outDir := parser.String("o", "out", &argparse.Option{Default: "extracted", Help: "Output directory (default: extracted)"})
	extrCfgStr := parser.String("c", "config", &argparse.Option{Help: "Configure extractors (see \"extractor config\" section)"})
	extrInclGlob := parser.String("i", "include", &argparse.Option{Help: "Extract only matching files (glob syntax, SEE MATCHING FILES SECTION)"})
	extrExclGlob := parser.String("x", "exclude", &argparse.Option{Help: "Exclude matching files (glob syntax, can be mixed with --include, SEE MATCHING FILES SECTION)"})
	//verbose := parser.Flag("v", "verbose", &argparse.Option{Help: "Provide more detailed status output"})
	knownFilesPath := parser.String("", "files_file", &argparse.Option{Help: "Path to a text file containing known file names"})
	knownTypesPath := parser.String("", "types_file", &argparse.Option{Help: "Path to a text file containing known type names"})
	if err := parser.Parse(nil); err != nil {
		if err == argparse.BreakAfterHelpError {
			os.Exit(0)
		}
		prt.Fatalf("%v", err)
	}

	extrCfg, err := parseExtractorConfig(*extrCfgStr)
	if err != nil {
		prt.Fatalf("%v", err)
	}
	extrEnabled := extrCfg["enable"]
	extrDisabled := extrCfg["disable"]

	extrIncl, err := glob.Compile(*extrInclGlob)
	if err != nil {
		prt.Fatalf("%v", err)
	}
	extrExcl, err := glob.Compile(*extrExclGlob)
	if err != nil {
		prt.Fatalf("%v", err)
	}

	runner := exec.NewRunner()
	if ok := runner.Add("ffmpeg", "-y", "-hide_banner", "-loglevel", "error"); !ok {
		prt.Warnf("FFmpeg not installed or found locally. Please install FFmpeg, or place ffmpeg.exe in the current folder to convert videos to MP4 and audio to a variety of formats. Without FFmpeg, videos will be saved as BIK and audio will be saved was WAV.")
	}
	if ok := runner.Add("magick"); !ok {
		prt.Warnf("ImageMagick not installed or found locally. Please install ImageMagick, or place magick.exe in the current folder to convert textures. Without magick, textures cannot be converted.")
	}

	if *gameDir == "" {
		hd2SteamPath, err := steampath.GetAppPath("553850", "Helldivers 2")
		if err == nil {
			prt.Infof("Using game found at: \"%v\"", hd2SteamPath)
			*gameDir = hd2SteamPath
		} else {
			if *gameDir == "" {
				prt.Errorf("Helldivers 2 Steam installation path not found: %v", err)
				prt.Fatalf("Unable to detect game install directory. Please specify the game directory manually using the '-g' option.")
			}
		}
	} else {
		prt.Infof("Game directory: \"%v\"", *gameDir)
	}

	prt.Infof("Output directory: \"%v\"", *outDir)

	if *knownFilesPath != "" {
		b, err := os.ReadFile(*knownFilesPath)
		if err != nil {
			prt.Fatalf("%v", err)
		}
		knownFilesStr = string(b)
	}
	if *knownTypesPath != "" {
		b, err := os.ReadFile(*knownTypesPath)
		if err != nil {
			prt.Fatalf("%v", err)
		}
		knownTypesStr = string(b)
	}

	createHashLookup := func(src string) map[stingray.Hash]string {
		res := make(map[stingray.Hash]string)
		sc := bufio.NewScanner(strings.NewReader(src))
		for sc.Scan() {
			s := strings.TrimSpace(sc.Text())
			if s != "" && !strings.HasPrefix(s, "//") {
				res[stingray.Sum64([]byte(s))] = s
			}
		}
		return res
	}

	knownFiles := createHashLookup(knownFilesStr)
	knownTypes := createHashLookup(knownTypesStr)

	// Hashes with known meaning, but not known source string
	knownTypes[stingray.Hash{Value: 0xeac0b497876adedf}] = "material"

	getFileNameAndType := func(id stingray.FileID) (name string, typ string) {
		var ok bool
		name, ok = knownFiles[id.Name]
		if !ok {
			name = id.Name.String()
		}
		typ, ok = knownTypes[id.Type]
		if !ok {
			typ = id.Type.String()
		}
		return
	}

	prt.Infof("Reading metadata...")
	dataDir, err := stingray.OpenDataDir(filepath.Join(*gameDir, "data"))
	if err != nil {
		prt.Fatalf("%v", err)
	}
	matchingFiles := make(map[stingray.FileID]*stingray.File)
	for id, file := range dataDir.Files {
		name, typ := getFileNameAndType(id)
		shouldIncl := true
		if *extrInclGlob != "" {
			shouldIncl = extrIncl.Match(name + "." + typ)
		}
		if *extrExclGlob != "" {
			if extrExcl.Match(name + "." + typ) {
				shouldIncl = false
			}
		}
		if !shouldIncl {
			continue
		}
		shouldExtract := true
		if extrEnabled != nil {
			shouldExtract = extrEnabled[typ] == "true"
		}
		if extrDisabled != nil {
			if extrDisabled[typ] == "true" {
				shouldExtract = false
			}
		}
		if !shouldExtract {
			continue
		}
		matchingFiles[id] = file
	}
	if *extrInclGlob != "" || *extrExclGlob != "" || len(extrEnabled) != 0 || len(extrDisabled) != 0 {
		prt.Infof("%v/%v game files match", len(matchingFiles), len(dataDir.Files))
	}
	prt.Infof("Extracting files...")
	numFile := 0
	numExtrFiles := 0
	for id, file := range matchingFiles {
		name, typ := getFileNameAndType(id)
		truncName := name
		if len(truncName) > 40 {
			truncName = "..." + truncName[len(truncName)-37:]
		}
		prt.Statusf("File %v/%v: %v (%v)", numFile+1, len(matchingFiles), truncName, typ)
		cfg, ok := extrCfg[typ]
		if !ok {
			cfg = make(extractor.Config)
		}
		if ok, err := extractStingrayFile(*outDir, file, name, typ, cfg, runner, func(name, typ stingray.Hash) *stingray.File {
			return dataDir.Files[stingray.FileID{
				Name: name,
				Type: typ,
			}]
		}); err != nil {
			prt.Errorf("%v", err)
		} else if ok {
			numExtrFiles++
		}
		numFile++
	}
	prt.NoStatus()
	prt.Infof("Extracted %v/%v matching files", numExtrFiles, len(matchingFiles))
}
