package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Simple extractor PoC:
// Usage: go run extractor.go <source.flac> <tracknum>
// - extracts CUESHEET (metaflac or .cue), writes to temp dir
// - runs shnsplit (attempts direct; falls back to piping flac -d -c | shnsplit)
// - searches produced files for the requested track number
// - if result is WAV, encodes using flac -j N

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: poc-extract <file.flac> <tracknum>")
		os.Exit(2)
	}
	src := os.Args[1]
	trk, err := strconv.Atoi(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid track number")
		os.Exit(2)
	}

	cueText, err := getCueFromMetaflac(src)
	if err != nil || strings.TrimSpace(cueText) == "" {
		base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
		cuePath := filepath.Join(filepath.Dir(src), base+".cue")
		b, rerr := os.ReadFile(cuePath)
		if rerr != nil {
			fmt.Fprintln(os.Stderr, "no CUESHEET found via metaflac nor .cue file")
			os.Exit(1)
		}
		cueText = string(b)
	}

	tempDir, err := os.MkdirTemp("/tmp", "trackfs-extract-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create temp dir:", err)
		os.Exit(1)
	}
	// keep temp dir for inspection; caller may cleanup
	cuePath := filepath.Join(tempDir, "export.cue")
	if err := os.WriteFile(cuePath, []byte(cueText), 0644); err != nil {
		fmt.Fprintln(os.Stderr, "cannot write cue:", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "[INFO] using temp dir:", tempDir)

	// try direct shnsplit
	err = runShnSplitDirect(tempDir, cuePath, src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[WARN] shnsplit direct failed, trying pipe fallback:", err)
		err = runShnSplitPipe(tempDir, cuePath, src)
		if err != nil {
			fmt.Fprintln(os.Stderr, "shnsplit failed:", err)
			os.Exit(1)
		}
	}

	out, ferr := findTrackFile(tempDir, trk)
	if ferr != nil {
		fmt.Fprintln(os.Stderr, "cannot find extracted track:", ferr)
		os.Exit(1)
	}

	// If it's a WAV, encode to FLAC using flac -j
	ext := strings.ToLower(filepath.Ext(out))
	if ext == ".wav" {
		encPath := strings.TrimSuffix(out, ext) + ".flac"
		nproc := runtime.NumCPU()
		cmd := exec.Command("flac", "-j", strconv.Itoa(nproc), "-f", "-o", encPath, out)
		cmd.Stderr = os.Stderr
		cmd.Stdout = os.Stdout
		fmt.Fprintln(os.Stderr, "[INFO] encoding WAV to FLAC:", cmd.String())
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "flac encode failed:", err)
			os.Exit(1)
		}
		out = encPath
	}

	fmt.Println(out)
}

func getCueFromMetaflac(path string) (string, error) {
	// Try direct call first
	cmd := exec.Command("metaflac", "--show-tag=CUESHEET", path)
	b, err := cmd.Output()
	if err == nil {
		out := string(b)
		out = strings.ReplaceAll(out, "\r\n", "\n")
		if strings.HasPrefix(out, "CUESHEET=") {
			out = strings.TrimPrefix(out, "CUESHEET=")
			out = strings.TrimPrefix(out, "\"")
			out = strings.TrimSuffix(out, "\"\n")
		}
		if strings.TrimSpace(out) != "" {
			return out, nil
		}
	}

	// Fallback: chdir to the source's directory and run metaflac with basename.
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	oldwd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldwd) }()
	if err := os.Chdir(dir); err != nil {
		return "", err
	}
	cmd = exec.Command("metaflac", "--show-tag=CUESHEET", base)
	b, err = cmd.Output()
	if err != nil {
		return "", err
	}
	out := string(b)
	out = strings.ReplaceAll(out, "\r\n", "\n")
	if strings.HasPrefix(out, "CUESHEET=") {
		out = strings.TrimPrefix(out, "CUESHEET=")
		out = strings.TrimPrefix(out, "\"")
		out = strings.TrimSuffix(out, "\"\n")
	}
	return out, nil
}

func runShnSplitDirect(tempDir, cuePath, src string) error {
	// shnsplit -f cue -o flac src
	cmd := exec.Command("shnsplit", "-f", cuePath, "-o", "flac", src)
	cmd.Dir = tempDir
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	return cmd.Run()
}

func runShnSplitPipe(tempDir, cuePath, src string) error {
	// flac -d -c src | shnsplit -f cue -t "%n.%p" -o flac -
	dec := exec.Command("flac", "-d", "-c", src)
	split := exec.Command("shnsplit", "-f", cuePath, "-t", "%n.%p", "-o", "flac", "-")
	split.Dir = tempDir

	r, w := ioPipe()
	dec.Stdout = w
	split.Stdin = r
	dec.Stderr = os.Stderr
	split.Stdout = os.Stdout
	split.Stderr = os.Stderr

	if err := dec.Start(); err != nil {
		return err
	}
	if err := split.Start(); err != nil {
		_ = dec.Process.Kill()
		return err
	}
	if err := dec.Wait(); err != nil {
		_ = split.Process.Kill()
		return err
	}
	w.Close()
	if err := split.Wait(); err != nil {
		return err
	}
	return nil
}

func ioPipe() (*os.File, *os.File) {
	r, w, _ := os.Pipe()
	return r, w
}

func findTrackFile(dir string, track int) (string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	padded := fmt.Sprintf("%02d", track)
	numRe := regexp.MustCompile(`^0*([0-9]+)`) 
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// prefer flac files
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".flac" && ext != ".wav" {
			continue
		}
		if strings.HasPrefix(name, padded) {
			return filepath.Join(dir, name), nil
		}
		if m := numRe.FindStringSubmatch(name); m != nil {
			n, _ := strconv.Atoi(m[1])
			if n == track {
				return filepath.Join(dir, name), nil
			}
		}
	}
	// fallback: try any .flac
	for _, e := range ents {
		if !e.IsDir() && strings.ToLower(filepath.Ext(e.Name())) == ".flac" {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", errors.New("no extracted file found")
}
