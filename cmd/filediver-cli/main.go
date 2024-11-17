package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime/pprof"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hellflame/argparse"
	"github.com/jwalton/go-supportscolor"

	"github.com/xypwn/filediver/app"
	"github.com/xypwn/filediver/exec"
	"github.com/xypwn/filediver/hashes"
	"github.com/xypwn/filediver/stingray"
)

// extractionWorker handles file extraction in a separate goroutine
func extractionWorker(
	ctx context.Context,
	jobs <-chan extractionJob,
	results chan<- extractionResult,
	a *app.App,
	outDir string,
	extrCfg map[string]map[string]string,
	runner *exec.Runner,
) {
	for job := range jobs {
		success := false
		var err error
		_, err = a.ExtractFile(ctx, job.fileID, outDir, extrCfg, runner)
		if err == nil {
			success = true
		} else if !errors.Is(err, context.Canceled) {
			err = fmt.Errorf("failed to extract %v: %w", job.fileName, err)
		}

		results <- extractionResult{
			index:    job.index,
			fileName: job.fileName,
			success:  success,
			err:      err,
		}
	}
}

type extractionJob struct {
	index    int
	fileID   stingray.FileID
	fileName string
}

type extractionResult struct {
	index    int
	fileName string
	success  bool
	err      error
}

type extractionProgress struct {
	completed atomic.Int32
	total     int32
	lastFile  atomic.Pointer[string]
}

func newExtractionProgress(total int) *extractionProgress {
	return &extractionProgress{
		total: int32(total),
	}
}

func (p *extractionProgress) increment(fileName string) {
	p.completed.Add(1)
	p.lastFile.Store(&fileName)
}

func main() {
	prt := app.NewPrinter(
		supportscolor.Stderr().SupportsColor,
		os.Stderr,
		os.Stderr,
	)

	parser := argparse.NewParser("filediver", "An unofficial Helldivers 2 game asset extractor.", &argparse.ParserConfig{
		EpiLog: `matching files:
  Syntax is Glob (meaning * is supported)
  Basic format being matched is: <file_path>.<file_type> .
  file_path is the file path, or the file hash and
  file_type is the data type (see "extractors" section for a list of data types).
  examples:
    filediver -i "content/audio/*.wwise_stream"            extract all wwise_stream files in content/audio, or any subfolders
    filediver -i "{*.bik,*.wwise_stream,*.wwise_bank}"     extract all video and audio files (though easier with extractor config)
    filediver -i "content/audio/us/303183090.wwise_stream" extract one particular audio file

extractor config:
  basic format: filediver -c "<key1>:<opt1>=<val1>,<opt2>=<val2> <key2>:<opt1>,<opt2>"
  examples:
    filediver -c "enable:all"                extract ALL files, including raw files (i.e. files that can't be converted)
    filediver -c "enable:audio"              only extract audio
    filediver -c "enable:bik bik:format=bik" only extract bik files, but don't convert them to mp4
    filediver -c "audio:format=ogg"          convert audio to ogg instead of wav

performance options:
  -p, --parallel N    number of parallel extraction workers (default: 1)
  examples:
    filediver -c "enable:video" -p 4         extract all video files using 4 parallel workers
    filediver -c "enable:all" -p 8           extract everything using 8 parallel workers
` + app.ExtractorConfigHelpMessage(app.ConfigFormat),
		DisableDefaultShowHelp: true,
	})

	// Existing flags
	gameDir := parser.String("g", "gamedir", &argparse.Option{Help: "Helldivers 2 game directory"})
	modeList := parser.Flag("l", "list", &argparse.Option{Help: "List all files without extracting anything"})
	outDir := parser.String("o", "out", &argparse.Option{Default: "extracted", Help: "Output directory (default: extracted)"})
	extrCfgStr := parser.String("c", "config", &argparse.Option{Help: "Configure extractors (see \"extractor config\" section)"})
	extrInclGlob := parser.String("i", "include", &argparse.Option{Help: "Select only matching files (glob syntax, see matching files section)"})
	extrExclGlob := parser.String("x", "exclude", &argparse.Option{Help: "Exclude matching files from selection (glob syntax, can be mixed with --include, see matching files section)"})
	cpuProfile := parser.String("", "cpuprofile", &argparse.Option{Help: "Write CPU diagnostic profile to specified file"})
	knownHashesPath := parser.String("", "hashes_file", &argparse.Option{Help: "Path to a text file containing known file and type names"})

	// New parallel processing flag
	numWorkers := parser.Int("p", "parallel", &argparse.Option{
		Default: "1",
		Help:    fmt.Sprintf("Number of parallel extraction workers (default: 1)"),
	})

	if err := parser.Parse(nil); err != nil {
		if err == argparse.BreakAfterHelpError {
			os.Exit(0)
		}
		prt.Fatalf("%v", err)
	}

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			prt.Fatalf("%v", err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			prt.Fatalf("%v", err)
		}
		defer pprof.StopCPUProfile()
	}

	extrCfg, err := app.ParseExtractorConfig(app.ConfigFormat, *extrCfgStr)
	if err != nil {
		prt.Fatalf("%v", err)
	}

	// Create a Runner per worker to avoid contention
	var runners []*exec.Runner
	for i := 0; i < *numWorkers; i++ {
		runner := exec.NewRunner()
		if ok := runner.Add("ffmpeg", "-y", "-hide_banner", "-loglevel", "error"); !ok && i == 0 {
			prt.Warnf("FFmpeg not installed or found locally. Please install FFmpeg, or place ffmpeg.exe in the current folder to convert videos to MP4 and audio to a variety of formats. Without FFmpeg, videos will be saved as BIK and audio will be saved was WAV.")
		}
		runners = append(runners, runner)
	}
	defer func() {
		for _, r := range runners {
			r.Close()
		}
	}()

	if *gameDir == "" {
		var err error
		*gameDir, err = app.DetectGameDir()
		if err == nil {
			prt.Infof("Using game found at: \"%v\"", *gameDir)
		} else {
			prt.Errorf("Helldivers 2 Steam installation path not found: %v", err)
			prt.Fatalf("Unable to detect game install directory. Please specify the game directory manually using the '-g' option.")
		}
	} else {
		prt.Infof("Game directory: \"%v\"", *gameDir)
	}

	var knownHashes []string
	knownHashes = append(knownHashes, app.ParseHashes(hashes.Hashes)...)
	if *knownHashesPath != "" {
		b, err := os.ReadFile(*knownHashesPath)
		if err != nil {
			prt.Fatalf("%v", err)
		}
		knownHashes = append(knownHashes, app.ParseHashes(string(b))...)
	}

	if !*modeList {
		prt.Infof("Output directory: \"%v\"", *outDir)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel()
	}()

	a, err := app.OpenGameDir(ctx, *gameDir, knownHashes, func(curr, total int) {
		prt.Statusf("Reading metadata %.0f%%", float64(curr)/float64(total)*100)
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			prt.NoStatus()
			prt.Warnf("Metadata read canceled, exiting")
			return
		} else {
			prt.Errorf("%v", err)
		}
	}
	prt.NoStatus()

	files, err := a.MatchingFiles(*extrInclGlob, *extrExclGlob, app.ConfigFormat, extrCfg)
	if err != nil {
		prt.Fatalf("%v", err)
	}

	getFileName := func(id stingray.FileID) string {
		name, ok := a.Hashes[id.Name]
		if !ok {
			name = id.Name.String()
		}
		typ, ok := a.Hashes[id.Type]
		if !ok {
			typ = id.Type.String()
		}
		return name + "." + typ
	}

	var sortedFileIDs []stingray.FileID
	for id := range files {
		sortedFileIDs = append(sortedFileIDs, id)
	}
	sort.Slice(sortedFileIDs, func(i, j int) bool {
		return getFileName(sortedFileIDs[i]) < getFileName(sortedFileIDs[j])
	})

	{
		names := make(map[stingray.Hash]struct{})
		types := make(map[stingray.Hash]struct{})
		for id := range a.DataDir.Files {
			names[id.Name] = struct{}{}
			types[id.Type] = struct{}{}
		}
		numKnownNames := 0
		numKnownTypes := 0
		for k := range names {
			if _, ok := a.Hashes[k]; ok {
				numKnownNames++
			}
		}
		for k := range types {
			if _, ok := a.Hashes[k]; ok {
				numKnownTypes++
			}
		}
		prt.Infof(
			"Known hashes: names %.2f%%, types %.2f%%",
			float64(numKnownNames)/float64(len(names))*100,
			float64(numKnownTypes)/float64(len(types))*100,
		)
	}

	if *modeList {
		for _, id := range sortedFileIDs {
			fmt.Println(getFileName(id))
		}
	} else {
		if *numWorkers > 1 {
			prt.Infof("Extracting files using %d workers...", *numWorkers)
		} else {
			prt.Infof("Extracting files...")
		}

		if *numWorkers <= 1 {
			// Original sequential extraction logic
			numExtrFiles := 0
			for i, id := range sortedFileIDs {
				truncName := getFileName(id)
				if len(truncName) > 40 {
					truncName = "..." + truncName[len(truncName)-37:]
				}
				prt.Statusf("File %v/%v: %v", i+1, len(files), truncName)
				if _, err := a.ExtractFile(ctx, id, *outDir, extrCfg, runners[0]); err == nil {
					numExtrFiles++
				} else {
					if errors.Is(err, context.Canceled) {
						prt.NoStatus()
						prt.Warnf("Extraction canceled, exiting cleanly")
						return
					} else {
						prt.Errorf("%v", err)
					}
				}
			}
			prt.NoStatus()
			prt.Infof("Extracted %v/%v matching files", numExtrFiles, len(files))
		} else {
			// Parallel extraction logic
			jobs := make(chan extractionJob, *numWorkers)
			results := make(chan extractionResult, *numWorkers)
			progress := newExtractionProgress(len(sortedFileIDs))

			// Start progress reporter
			progressDone := make(chan struct{})
			go func() {
				defer close(progressDone)
				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()

				for {
					select {
					case <-ticker.C:
						completed := progress.completed.Load()
						if completed >= progress.total {
							return
						}
						lastFile := progress.lastFile.Load()
						if lastFile != nil {
							truncName := *lastFile
							if len(truncName) > 40 {
								truncName = "..." + truncName[len(truncName)-37:]
							}
							prt.Statusf("Progress: %.1f%% (%d/%d) | Current: %s",
								float64(completed)/float64(progress.total)*100,
								completed,
								progress.total,
								truncName)
						}
					case <-ctx.Done():
						return
					}
				}
			}()

			// Start worker pool
			var wg sync.WaitGroup
			for i := 0; i < *numWorkers; i++ {
				wg.Add(1)
				go func(workerID int) {
					defer wg.Done()
					extractionWorker(ctx, jobs, results, a, *outDir, extrCfg, runners[workerID])
				}(i)
			}

			// Start result collector
			numExtrFiles := 0
			errList := make([]error, 0)
			done := make(chan struct{})

			go func() {
				for i := 0; i < len(sortedFileIDs); i++ {
					result := <-results

					if result.success {
						numExtrFiles++
						progress.increment(result.fileName)
					} else if result.err != nil {
						if errors.Is(result.err, context.Canceled) {
							cancel()
						} else {
							errList = append(errList, result.err)
						}
					}
				}
				close(done)
			}()

			// Send jobs
			for i, id := range sortedFileIDs {
				select {
				case <-ctx.Done():
					goto cleanup
				default:
					jobs <- extractionJob{
						index:    i,
						fileID:   id,
						fileName: getFileName(id),
					}
				}
			}

		cleanup:
			close(jobs)
			wg.Wait()
			close(results)
			<-done
			<-progressDone

			prt.NoStatus()
			for _, err := range errList {
				prt.Errorf("%v", err)
			}
			prt.Infof("Extracted %v/%v matching files", numExtrFiles, len(files))
		}
	}
}
