package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
)

// Simple FUSE prototype exposing a single album as a directory with per-track files
// All extraction logic is now inline (no external poc-extract dependency)

// TrackGen caches generation state for a single track.
// sync.Once ensures extraction runs exactly once per TrackGen instance.
type TrackGen struct {
	mu    sync.Mutex
	once  sync.Once
	src   string
	track int
	path  string
	err   error
}

func (t *TrackGen) GetPath() (string, error) {
	t.once.Do(t.generate)
	return t.path, t.err
}

func (t *TrackGen) generate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.path != "" || t.err != nil {
		return
	}
	t.path, t.err = ensureTrackGenerated(t.src, t.track)
}

func main() {
	// Allow flags anywhere (move --length and its value to front)
	args := os.Args[1:]
	flagsFront := make([]string, 0)
	others := make([]string, 0)
	for i := 0; i < len(args); i++ {
		if args[i] == "--length" && i+1 < len(args) {
			flagsFront = append(flagsFront, args[i], args[i+1])
			i++
		} else {
			others = append(others, args[i])
		}
	}
	newArgs := append(flagsFront, others...)
	os.Args = append([]string{os.Args[0]}, newArgs...)

	flag.Usage = func() { fmt.Fprintf(os.Stderr, "usage: poc-fuse [--length N] <source.flac> <mountpoint>\n") }
	maxLen := flag.Int("length", 0, "max title length in characters (0 = no limit)")
	flag.Parse()
	if flag.NArg() < 2 {
		flag.Usage()
		os.Exit(2)
	}
	src := flag.Arg(0)
	mountpoint := flag.Arg(1)
	if flag.NArg() >= 3 {
		// support root + relpath style
		src = filepath.Join(flag.Arg(0), flag.Arg(1))
		mountpoint = flag.Arg(2)
	}

	tracks := getTracks(src)
	if len(tracks) == 0 {
		log.Fatalf("no tracks parsed for %s", src)
	}
	albumBase := filepath.Base(src)
	albumName := albumBase[:len(albumBase)-len(filepath.Ext(albumBase))]
	// apply truncation if requested
	if *maxLen > 0 {
		for i := range tracks {
			tracks[i].Title = truncateRunes(tracks[i].Title, *maxLen)
		}
	}

	c, err := fuse.Mount(
		mountpoint,
		fuse.ReadOnly(),
		fuse.FSName("trackfs-poc"),
		fuse.Subtype("trackfs"),
	)
	if err != nil {
		log.Fatalf("mount error: %v", err)
	}
	defer fuse.Unmount(mountpoint)

	srv := fs.New(c, nil)
	tree := &RootDir{AlbumName: albumName, Tracks: tracks, Source: src}
	go func() {
		if err := srv.Serve(&FS{root: tree}); err != nil {
			log.Fatalf("serve error: %v", err)
		}
	}()

	// keep running until interrupted
	select {}
}

// RootDir implements fs.Node + fs.HandleReadDirAller
type RootDir struct {
	AlbumName string
	Tracks    []struct {
		Number int
		Title  string
	}
	Source string
}

func (r *RootDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0555
	return nil
}

func (r *RootDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	ents := []fuse.Dirent{{Name: r.AlbumName, Type: fuse.DT_Dir}}
	return ents, nil
}

// AlbumDir lists tracks
type AlbumDir struct {
	AlbumName string
	Tracks    []struct {
		Number int
		Title  string
	}
	Source string
}

func (d *AlbumDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0555
	return nil
}

func (d *AlbumDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	ents := make([]fuse.Dirent, 0, len(d.Tracks))
	for _, t := range d.Tracks {
		name := fmt.Sprintf("%03d.%s.flac", t.Number, sanitizeForFs(t.Title))
		ents = append(ents, fuse.Dirent{Name: name, Type: fuse.DT_File})
	}
	return ents, nil
}

// Lookup: root->album dir, album->track file
func (r *RootDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	if name == r.AlbumName {
		return &AlbumDir{AlbumName: r.AlbumName, Tracks: r.Tracks, Source: r.Source}, nil
	}
	return nil, fuse.ENOENT
}

func (d *AlbumDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	// match by generated name
	for _, t := range d.Tracks {
		expected := fmt.Sprintf("%03d.%s.flac", t.Number, sanitizeForFs(t.Title))
		if name == expected {
			return &TrackFile{Number: t.Number, Title: t.Title, Source: d.Source}, nil
		}
	}
	return nil, fuse.ENOENT
}

// TrackFile: only Attr supported in PoC
type TrackFile struct {
	Number int
	Title  string
	Source string
	gen    *TrackGen // lazy generation state
}

func (f *TrackFile) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0444
	// generate on first Attr access (lazy)
	path, err := f.getGen().GetPath()
	if err == nil && path != "" {
		if st, err := os.Stat(path); err == nil {
			a.Size = uint64(st.Size())
		}
	}
	a.Mtime = time.Now()
	return nil
}

func (f *TrackFile) getGen() *TrackGen {
	if f.gen == nil {
		f.gen = &TrackGen{src: f.Source, track: f.Number}
	}
	return f.gen
}

// Open: ensure track is generated and return a handle
func (f *TrackFile) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	// generate on demand (safe to call multiple times due to sync.Once)
	path, err := f.getGen().GetPath()
	if err != nil {
		log.Printf("generate track #%d failed: %v", f.Number, err)
		return nil, fuse.EIO
	}
	return &TrackHandle{Path: path}, nil
}

// TrackHandle supports Read
type TrackHandle struct {
	Path string
	fd   *os.File
}

func (h *TrackHandle) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	// open file lazily
	if h.fd == nil {
		f, err := os.Open(h.Path)
		if err != nil {
			return fuse.EIO
		}
		h.fd = f
	}
	buf := make([]byte, req.Size)
	n, err := h.fd.ReadAt(buf, req.Offset)
	if err != nil && err != io.EOF {
		return fuse.EIO
	}
	resp.Data = buf[:n]
	return nil
}

func (h *TrackHandle) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	if h.fd != nil {
		h.fd.Close()
		h.fd = nil
	}
	return nil
}

// ============================================================
// Extraction Engine (inlined from extractor.go)
// ============================================================

func ensureTrackGenerated(src string, track int) (string, error) {
	// create temp dir per source
	tmpRoot := "/tmp"
	tmpDir, err := os.MkdirTemp(tmpRoot, "trackfs-extract-")
	if err != nil {
		return "", err
	}
	// call extraction logic inline
	cueText, err := getCueFromMetaflac(src)
	if err != nil || strings.TrimSpace(cueText) == "" {
		base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
		cuePath := filepath.Join(filepath.Dir(src), base+".cue")
		b, rerr := os.ReadFile(cuePath)
		if rerr != nil {
			return "", fmt.Errorf("no CUESHEET found via metaflac nor .cue file")
		}
		cueText = string(b)
	}

	fmt.Fprintln(os.Stderr, "[INFO] using temp dir:", tmpDir)

	// write cue to temp dir
	cueFile := filepath.Join(tmpDir, "export.cue")
	if err := os.WriteFile(cueFile, []byte(cueText), 0644); err != nil {
		return "", fmt.Errorf("cannot write cue: %w", err)
	}

	// try direct shnsplit
	err = runShnSplitDirect(tmpDir, cueFile, src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[WARN] shnsplit direct failed, trying pipe fallback:", err)
		err = runShnSplitPipe(tmpDir, cueFile, src)
		if err != nil {
			return "", fmt.Errorf("shnsplit failed: %w", err)
		}
	}

	// find track file
	return findTrackFile(tmpDir, track)
}

// getCueFromMetaflac extracts CUESHEET from FLAC metadata
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
	return "", fmt.Errorf("no extracted file found")
}

// encodeWavToFlac converts WAV to FLAC
func encodeWavToFlac(wavPath string) (string, error) {
	encPath := strings.TrimSuffix(wavPath, ".wav") + ".flac"
	nproc := runtime.NumCPU()
	cmd := exec.Command("flac", "-j", strconv.Itoa(nproc), "-f", "-o", encPath, wavPath)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	fmt.Fprintln(os.Stderr, "[INFO] encoding WAV to FLAC:", cmd.String())
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("flac encode failed: %w", err)
	}
	return encPath, nil
}

// ============================================================
// Track parsing (from original fuse_main.go)
// ============================================================

// getTracks: call metaflac directly and parse CUE
func getTracks(src string) []struct {
	Number int
	Title  string
} {
	cueText, err := getCueFromMetaflac(src)
	if err != nil || strings.TrimSpace(cueText) == "" {
		// fallback to .cue file
		base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
		cuePath := filepath.Join(filepath.Dir(src), base+".cue")
		b, rerr := os.ReadFile(cuePath)
		if rerr != nil {
			log.Printf("no CUESHEET found via metaflac nor .cue file")
			return []struct {
				Number int
				Title  string
			}{}
		}
		cueText = string(b)
	}

	if strings.TrimSpace(cueText) == "" {
		log.Printf("no CUESHEET found")
		return []struct {
			Number int
			Title  string
		}{}
	}

	tracks := parseCue(cueText)
	res := make([]struct {
		Number int
		Title  string
	}, len(tracks))
	for i, t := range tracks {
		res[i].Number = t.Number
		res[i].Title = sanitizeForFs(t.Title)
	}
	return res
}

// parseCue parses CUE sheet text into track list
func parseCue(r string) []struct {
	Number int
	Title  string
} {
	s := bufio.NewScanner(strings.NewReader(r))
	trackRe := regexp.MustCompile(`^\s*TRACK\s+([0-9]+)`)
	titleRe := regexp.MustCompile(`^\s*TITLE\s+"(.*)"`)
	indexRe := regexp.MustCompile(`^\s*INDEX\s+01\s+([0-9]{2}:[0-9]{2}:[0-9]{2})`)

	var tracks []struct {
		Number int
		Title  string
	}
	var cur struct {
		Number int
		Title  string
	}
	inTrack := false
	for s.Scan() {
		line := s.Text()
		if m := trackRe.FindStringSubmatch(line); m != nil {
			if inTrack {
				tracks = append(tracks, cur)
			}
			inTrack = true
			num, _ := strconv.Atoi(m[1])
			cur = struct {
				Number int
				Title  string
			}{Number: num}
			continue
		}
		if !inTrack {
			continue
		}
		if m := titleRe.FindStringSubmatch(line); m != nil {
			cur.Title = m[1]
			continue
		}
		if m := indexRe.FindStringSubmatch(line); m != nil {
			continue
		}
	}
	if inTrack {
		tracks = append(tracks, cur)
	}
	return tracks
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func sanitizeForFs(s string) string {
	// normalize and replace forbidden characters
	s = strings.TrimSpace(s)
	re := regexp.MustCompile("[\\x00-\\x1F\\x7F]+")
	s = re.ReplaceAllString(s, "")
	forbidden := []string{string(os.PathSeparator), "/", "\\", ":", "*", "?", "\"", "<", ">", "|"}
	for _, ch := range forbidden {
		s = strings.ReplaceAll(s, ch, "_")
	}
	// also collapse multiple underscores
	s = regexp.MustCompile("_+").ReplaceAllString(s, "_")
	return s
}

// FS wrapper implementing fs.FS
type FS struct {
	root *RootDir
}

func (f *FS) Root() (fs.Node, error) {
	return f.root, nil
}

// Ensure interfaces are implemented
var _ fs.FS = (*FS)(nil)
var _ fs.Node = (*RootDir)(nil)
var _ fs.HandleReadDirAller = (*RootDir)(nil)
var _ fs.NodeStringLookuper = (*RootDir)(nil)
var _ fs.Node = (*AlbumDir)(nil)
var _ fs.HandleReadDirAller = (*AlbumDir)(nil)
var _ fs.NodeStringLookuper = (*AlbumDir)(nil)
var _ fs.Node = (*TrackFile)(nil)
