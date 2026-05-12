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

// ============================================================
// Global track cache — avoids re-extraction across all tracks
// ============================================================

type TrackCache struct {
	mu sync.Mutex
	m  map[string]string // key "src|track" → path
}

var globalCache = &TrackCache{m: make(map[string]string)}

func (c *TrackCache) Get(src string, track int) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := fmt.Sprintf("%s|%d", src, track)
	p, ok := c.m[k]
	return p, ok
}

func (c *TrackCache) Set(src string, track int, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[fmt.Sprintf("%s|%d", src, track)] = path
}

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

	// Check global cache first (another TrackGen might have generated it)
	if p, ok := globalCache.Get(t.src, t.track); ok {
		t.path = p
		return
	}

	if t.path != "" || t.err != nil {
		return
	}
	t.path, t.err = ensureTrackGenerated(t.src, t.track)
	if t.err == nil && t.path != "" {
		globalCache.Set(t.src, t.track, t.path)
	}
}

// DirEntry represents one item discovered in the source directory
type DirEntry struct {
	Name     string // display name in mount
	IsDir    bool   // true for album dirs (CUE-containing FLACs)
	OrigPath string // original file path on disk (for passthrough)
	Tracks   []struct {
		Number int
		Title  string
	} // tracks if album dir
	Source string // source FLAC path for extraction
}

// ============================================================
// Directory scanning
// ============================================================

// scanSourceDirectory scans the source and returns a list of DirEntries
func scanSourceDirectory(srcDir string) ([]DirEntry, error) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, fmt.Errorf("read source directory: %w", err)
	}

	var result []DirEntry
	seenAlbums := make(map[string]bool) // deduplicate by album name

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))

		if ext == ".flac" {
			flacPath := filepath.Join(srcDir, name)
			tracks := getTracks(flacPath)

			if len(tracks) > 0 {
				// CUE-containing FLAC → album directory
				base := strings.TrimSuffix(name, ext)
				albumName := sanitizeForFs(base)
				// apply truncation if needed (handled in main with --length)
				if seenAlbums[albumName] {
					// duplicate album name — skip (already mounted)
					continue
				}
				seenAlbums[albumName] = true
				result = append(result, DirEntry{
					Name:     albumName,
					IsDir:    true,
					OrigPath: flacPath,
					Tracks:   tracks,
					Source:   flacPath,
				})
			} else {
				// No CUE → passthrough the FLAC file
				result = append(result, DirEntry{
					Name:     name,
					IsDir:    false,
					OrigPath: flacPath,
				})
			}
		} else {
			// Other files → passthrough as-is
			result = append(result, DirEntry{
				Name:     name,
				IsDir:    false,
				OrigPath: filepath.Join(srcDir, name),
			})
		}
	}

	return result, nil
}

// ============================================================
// FUSE nodes
// ============================================================

// RootDir: top-level directory listing all albums + files
type RootDir struct {
	Entries []DirEntry
}

func (r *RootDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0555
	return nil
}

func (r *RootDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	ents := make([]fuse.Dirent, 0, len(r.Entries))
	for _, e := range r.Entries {
		ents = append(ents, fuse.Dirent{
			Name: e.Name,
			Type: mapBoolToType(e.IsDir),
		})
	}
	return ents, nil
}

func (r *RootDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	for _, e := range r.Entries {
		if e.Name == name {
			if e.IsDir {
				return &AlbumDir{
					AlbumName: name,
					Tracks:    e.Tracks,
					Source:    e.Source,
				}, nil
			}
			return &PassthroughFile{Path: e.OrigPath}, nil
		}
	}
	return nil, fuse.ENOENT
}

func mapBoolToType(isDir bool) fuse.DirentType {
	if isDir {
		return fuse.DT_Dir
	}
	return fuse.DT_File
}

// AlbumDir: directory listing tracks of a single FLAC album
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

func (d *AlbumDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	for _, t := range d.Tracks {
		expected := fmt.Sprintf("%03d.%s.flac", t.Number, sanitizeForFs(t.Title))
		if name == expected {
			return &TrackFile{Number: t.Number, Title: t.Title, Source: d.Source}, nil
		}
	}
	return nil, fuse.ENOENT
}

// TrackFile: extracted track — lazy generation with global cache
type TrackFile struct {
	Number int
	Title  string
	Source string
	gen    *TrackGen
}

func (f *TrackFile) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0444
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

func (f *TrackFile) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	path, err := f.getGen().GetPath()
	if err != nil {
		log.Printf("generate track #%d from %s failed: %v", f.Number, filepath.Base(f.Source), err)
		return nil, fuse.EIO
	}
	return &TrackHandle{Path: path}, nil
}

// PassthroughFile: direct passthrough for non-CUE FLACs and other files
type PassthroughFile struct{ Path string }

func (f *PassthroughFile) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0444
	if st, err := os.Stat(f.Path); err == nil {
		a.Size = uint64(st.Size())
		a.Mtime = st.ModTime()
	}
	return nil
}

func (f *PassthroughFile) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	fd, err := os.Open(f.Path)
	if err != nil {
		return nil, fuse.EIO
	}
	return &PassthroughHandle{fd: fd}, nil
}

// PassthroughHandle: reads directly from the original file
type PassthroughHandle struct {
	fd *os.File
}

func (h *PassthroughHandle) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	n, err := h.fd.ReadAt(req.Data, req.Offset)
	if err != nil && err != io.EOF {
		return fuse.EIO
	}
	resp.Data = req.Data[:n]
	return nil
}

func (h *PassthroughHandle) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	if h.fd != nil {
		h.fd.Close()
		h.fd = nil
	}
	return nil
}

// TrackHandle: reads from an extracted track file
type TrackHandle struct {
	Path string
	fd   *os.File
}

func (h *TrackHandle) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
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
// main()
// ============================================================

func main() {
	// Allow flags anywhere
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

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: poc-fuse [--length N] <source.flac|source_dir> <mountpoint>\n")
		fmt.Fprintf(os.Stderr, "  single file: poc-fuse album.flac /mount/point\n")
		fmt.Fprintf(os.Stderr, "  directory:   poc-fuse /path/to/dir  /mount/point\n")
	}
	maxLen := flag.Int("length", 0, "max title length in characters (0 = no limit)")
	flag.Parse()
	if flag.NArg() < 2 {
		flag.Usage()
		os.Exit(2)
	}

	src := flag.Arg(0)
	mountpoint := flag.Arg(1)

	// Detect if src is a directory or a file
	info, err := os.Stat(src)
	if err != nil {
		log.Fatalf("source not found: %v", err)
	}

	var tree *RootDir

	if info.IsDir() {
		// Directory mode: scan all FLACs
		log.Printf("[INFO] scanning directory: %s", src)
		entries, err := scanSourceDirectory(src)
		if err != nil {
			log.Fatalf("scan failed: %v", err)
		}
		log.Printf("[INFO] found %d entries", len(entries))

		// Apply truncation to album titles
		if *maxLen > 0 {
			for i := range entries {
				if entries[i].IsDir {
					for j := range entries[i].Tracks {
						entries[i].Tracks[j].Title = truncateRunes(entries[i].Tracks[j].Title, *maxLen)
					}
				}
			}
		}

		tree = &RootDir{Entries: entries}
	} else {
		// Single file mode (backward compat)
		tracks := getTracks(src)
		if len(tracks) == 0 {
			log.Fatalf("no tracks parsed for %s", src)
		}
		// Apply truncation
		if *maxLen > 0 {
			for i := range tracks {
				tracks[i].Title = truncateRunes(tracks[i].Title, *maxLen)
			}
		}
		albumName := sanitizeForFs(strings.TrimSuffix(filepath.Base(src), filepath.Ext(src)))
		tree = &RootDir{
			Entries: []DirEntry{{
				Name:     albumName,
				IsDir:    true,
				OrigPath: src,
				Tracks:   tracks,
				Source:   src,
			}},
		}
	}

	// Mount
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
	go func() {
		if err := srv.Serve(&FS{root: tree}); err != nil {
			log.Fatalf("serve error: %v", err)
		}
	}()

	select {}
}

// ============================================================
// Extraction Engine
// ============================================================

func ensureTrackGenerated(src string, track int) (string, error) {
	tmpRoot := "/tmp"
	tmpDir, err := os.MkdirTemp(tmpRoot, "trackfs-extract-")
	if err != nil {
		return "", err
	}

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

	cueFile := filepath.Join(tmpDir, "export.cue")
	if err := os.WriteFile(cueFile, []byte(cueText), 0644); err != nil {
		return "", fmt.Errorf("cannot write cue: %w", err)
	}

	err = runShnSplitDirect(tmpDir, cueFile, src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[WARN] shnsplit direct failed, trying pipe fallback:", err)
		err = runShnSplitPipe(tmpDir, cueFile, src)
		if err != nil {
			return "", fmt.Errorf("shnsplit failed: %w", err)
		}
	}

	return findTrackFile(tmpDir, track)
}

// getCueFromMetaflac extracts CUESHEET from FLAC metadata
func getCueFromMetaflac(path string) (string, error) {
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
	cmd := exec.Command("shnsplit", "-f", cuePath, "-o", "flac", src)
	cmd.Dir = tempDir
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	return cmd.Run()
}

func runShnSplitPipe(tempDir, cuePath, src string) error {
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
	for _, e := range ents {
		if !e.IsDir() && strings.ToLower(filepath.Ext(e.Name())) == ".flac" {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("no extracted file found")
}

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
// Track parsing
// ============================================================

func getTracks(src string) []struct {
	Number int
	Title  string
} {
	cueText, err := getCueFromMetaflac(src)
	if err != nil || strings.TrimSpace(cueText) == "" {
		base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
		cuePath := filepath.Join(filepath.Dir(src), base+".cue")
		b, rerr := os.ReadFile(cuePath)
		if rerr != nil {
			log.Printf("no CUESHEET found via metaflac nor .cue file")
			return nil
		}
		cueText = string(b)
	}

	if strings.TrimSpace(cueText) == "" {
		log.Printf("no CUESHEET found")
		return nil
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
	s = strings.TrimSpace(s)
	re := regexp.MustCompile("[\\x00-\\x1F\\x7F]+")
	s = re.ReplaceAllString(s, "")
	forbidden := []string{string(os.PathSeparator), "/", "\\", ":", "*", "?", "\"", "<", ">", "|"}
	for _, ch := range forbidden {
		s = strings.ReplaceAll(s, ch, "_")
	}
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
