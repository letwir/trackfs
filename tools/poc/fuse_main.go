package main

import (
	"bytes"
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
	"strconv"
	"strings"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
)

// Simple FUSE prototype exposing a single album as a directory with per-track files

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
	Tracks    []struct{ Number int; Title string }
	Source    string
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
	Tracks    []struct{ Number int; Title string }
	Source    string
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
	Path   string // path to generated track file (populated on open)
}

func (f *TrackFile) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0444
	// if generated file exists, return its size
	if f.Path != "" {
		if st, err := os.Stat(f.Path); err == nil {
			a.Size = uint64(st.Size())
		}
	}
	a.Mtime = time.Now()
	return nil
}

// Open: ensure track is generated and return a handle
func (f *TrackFile) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	// generate on demand
	outPath, err := ensureTrackGenerated(f.Source, f.Number)
	if err != nil {
		log.Printf("generate track failed: %v", err)
		return nil, fuse.EIO
	}
	f.Path = outPath
	return &TrackHandle{Path: outPath}, nil
}

// TrackHandle supports Read
type TrackHandle struct{
	Path string
	fd   *os.File
}

func (h *TrackHandle) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	// open file lazily
	if h.fd == nil {
		f, err := os.Open(h.Path)
		if err != nil { return fuse.EIO }
		h.fd = f
	}
	buf := make([]byte, req.Size)
	n, err := h.fd.ReadAt(buf, req.Offset)
	if err != nil && err != io.EOF { return fuse.EIO }
	resp.Data = buf[:n]
	return nil
}

func (h *TrackHandle) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	if h.fd != nil { h.fd.Close(); h.fd = nil }
	return nil
}

// getTracks: call poc-metaflac and parse JSON
func getTracks(src string) []struct{ Number int; Title string } {
	// Try metaflac directly
	mf, err := exec.LookPath("metaflac")
	var cueText string
	if err == nil {
		cmd := exec.Command(mf, "--show-tag=CUESHEET", src)
		var out bytes.Buffer
		var errbuf bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errbuf
		if err := cmd.Run(); err == nil {
			cueText = out.String()
		} else {
			log.Printf("metaflac failed (direct): %v; stderr: %s", err, strings.TrimSpace(errbuf.String()))
			// try running metaflac from the file's directory with basename argument
			base := filepath.Base(src)
			cmd2 := exec.Command(mf, "--show-tag=CUESHEET", base)
			cmd2.Dir = filepath.Dir(src)
			var out2 bytes.Buffer
			var errbuf2 bytes.Buffer
			cmd2.Stdout = &out2
			cmd2.Stderr = &errbuf2
			if err2 := cmd2.Run(); err2 == nil {
				cueText = out2.String()
			} else {
				log.Printf("metaflac failed (chdir): %v; stderr: %s", err2, strings.TrimSpace(errbuf2.String()))
			}
		}
	} else {
		log.Printf("metaflac not found in PATH: %v", err)
	}

	if strings.TrimSpace(cueText) == "" {
		// fallback to .cue file
		base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
		cuePath := filepath.Join(filepath.Dir(src), base+".cue")
		if b, rerr := os.ReadFile(cuePath); rerr == nil {
			cueText = string(b)
		}
	}

	if strings.TrimSpace(cueText) == "" {
		log.Printf("no CUESHEET found via metaflac nor .cue file")
		return []struct{ Number int; Title string }{}
	}

	tracks := parseCue(cueText)
	res := make([]struct{ Number int; Title string }, len(tracks))
	for i, t := range tracks {
		res[i].Number = t.Number
		res[i].Title = sanitizeForFs(t.Title)
	}
	return res
}

// parseCue copied from poc-metaflac implementation
func parseCue(r string) []struct{ Number int; Title string } {
	s := bufio.NewScanner(strings.NewReader(r))
	trackRe := regexp.MustCompile(`^\s*TRACK\s+([0-9]+)`) 
	titleRe := regexp.MustCompile(`^\s*TITLE\s+"(.*)"`) 
	indexRe := regexp.MustCompile(`^\s*INDEX\s+01\s+([0-9]{2}:[0-9]{2}:[0-9]{2})`)

	var tracks []struct{ Number int; Title string }
	var cur struct{ Number int; Title string }
	inTrack := false
	for s.Scan() {
		line := s.Text()
		if m := trackRe.FindStringSubmatch(line); m != nil {
			if inTrack {
				tracks = append(tracks, cur)
			}
			inTrack = true
			num, _ := strconv.Atoi(m[1])
			cur = struct{ Number int; Title string }{Number: num}
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
			// ignore index value for name; just continue
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
	// simple approach here
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

// ensureTrackGenerated generates a track file using extractor PoC and returns path
func ensureTrackGenerated(src string, track int) (string, error) {
	// create temp dir per source
	tmpRoot := "/tmp"
	tmpDir, err := os.MkdirTemp(tmpRoot, "trackfs-extract-")
	if err != nil {
		return "", err
	}
	// call poc-extract (or run logic inline); use existing poc-extract binary if present
	exPath := "./poc-extract"
	if p, err := exec.LookPath(exPath); err == nil {
		cmd := exec.Command(p, src, strconv.Itoa(track))
		cmd.Dir = tmpDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return "", err
		}
		// try to find result in tmpDir
		ents, derr := os.ReadDir(tmpDir)
		if derr != nil { return "", derr }
		for _, e := range ents {
			if !e.IsDir() {
				ext := strings.ToLower(filepath.Ext(e.Name()))
				if ext == ".flac" || ext == ".wav" { return filepath.Join(tmpDir, e.Name()), nil }
			}
		}
		return "", fmt.Errorf("no output file from extractor")
	}
	// fallback: run same logic inline (call shnsplit)
	cmd := exec.Command("shnsplit", "-f", "-", "-o", "flac")
	_ = cmd
	return "", fmt.Errorf("extractor binary not found")
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
