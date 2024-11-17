<div align="center">

# FileDiver

[![GitHub Actions Workflow Status](https://img.shields.io/github/actions/workflow/status/xypwn/filediver/.github%2Fworkflows%2Fbuild-release.yml)](https://github.com/xypwn/filediver/actions)
[![Scrutinizer quality (GitHub/Bitbucket)](https://img.shields.io/scrutinizer/quality/g/xypwn/filediver)](https://scrutinizer-ci.com/g/xypwn/filediver)
[![GitHub License](https://img.shields.io/github/license/xypwn/filediver)](https://opensource.org/license/bsd-3-clause)

[![GitHub Release](https://img.shields.io/github/v/release/xypwn/filediver)](https://github.com/xypwn/filediver/releases/latest/)
[![GitHub Downloads (all assets, all releases)](https://img.shields.io/github/downloads/xypwn/filediver/total)](https://github.com/xypwn/filediver/releases/latest/)

An unofficial Helldivers 2 game asset extractor.
</div>

## Download
- [Windows (64-bit)](https://github.com/xypwn/filediver/releases/latest/download/filediver-windows-amd64.zip)
- [Linux (64-bit)](https://github.com/xypwn/filediver/releases/latest/download/filediver-linux-amd64.tar.gz)

**Extract the achive into a new folder.**

The program is called "filediver.exe" (or just "filediver" on Linux). See [usage](#usage).

<details>
<summary>What is "ffmpeg.exe"?</summary>

"ffmpeg.exe" ([FFmpeg](https://ffmpeg.org/)) is used for converting video and audio files. It is downloaded from an official source by the [GitHub workflow](https://github.com/xypwn/filediver/blob/master/.github/workflows/build-release.yml) that generates the .zip archive you can download.

You only need to keep it in the folder if you don't have it installed on your computer already.
</details>

## Usage
### Windows
Navigate to the folder where you unpacked the program into. `SHIFT`+`Right-Click` **in** the folder and select "Open in PowerShell".

In PowerShell/Terminal, run `./filediver -h` to get a list of options.

### Here are some example commands:

Simply running the app should automatically detect your installation directory and dump all files into the "extracted" directory in your current folder:
```sh
./filediver
```

Print a detailed description of all command line options:
```sh
./filediver -h
```

Extract the files into a directory called "custom_dir":
```sh
./filediver -o "custom_dir"
```

Extract only video files:
```sh
./filediver -c "enable:video"
```

Extract only audio files using 4 parallel workers for faster processing:
```sh
./filediver -c "enable:video" -p 4
```

Extract the Super Earth anthem as mp3:
```sh
./filediver -c "audio:format=mp3" -i "content/audio/291227525.wwise_stream"
```

## Features
### File Types/Formats
- **Audio**: Audiokinetic wwise bnk/wem; automatically converted to WAV; other formats require FFmpeg
- **Video**: Bink; automatically converted to MP4 via FFmpeg (shipped with Windows binary)
- **Textures**: Direct Draw Surface (.dds); automatically converted to PNG
- **Models (WIP)**: Stingray Unit; automatically converted to GLB (=glTF); can be imported into [Blender](https://www.blender.org/); for importing bones, see [Importing Bones](#importing-bones)

Planned: animations

### Importing Bones
When importing the .glb into blender, you need to change the "Bone Dir" option from "Blender" to "Temperance", or you will see huge spheres for bones.

## Credits/Links
This app builds on a lot of work from other people. This includes:
- [Hellextractor by Xaymar](https://github.com/Xaymar/Hellextractor)
	- Basic binary file structure
	- Unhashed resource names/types (.txt files)
- [vgmstream](https://github.com/vgmstream/vgmstream), [ww2ogg by hcs](https://github.com/hcs64/ww2ogg) and [bnkextr by eXpl0it3r](https://github.com/eXpl0it3r/bnkextr)
	- Wwise audio formats
- [ImageMagick](https://imagemagick.org)
	- DDS texture decoding

Some useful discussion on the topic of HD2 resource extraction: https://reshax.com/topic/507-helldivers-2-model-extraction-help/

## Hacking
- Install [Go](https://go.dev/dl/)
- `go run ./cmd/filediver-cli`

## License
Copyright (c) Darwin Schuppan and contributors

FileDiver is licensed under the 3-Clause BSD License (https://opensource.org/license/bsd-3-clause).
