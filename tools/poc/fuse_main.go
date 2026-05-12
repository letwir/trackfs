package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
)

// Simple FUSE prototype exposing a single album as a directory with per-track files

func main() {
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
		if err := srv.Serve(tree); err != nil {
			log.Fatalf("serve error: %v", err)
		}
	}()

	// wait until mount is ready or fail
	<-c.Ready
	if err := c.MountError; err != nil {
		log.Fatalf("mount failed: %v", err)
	}
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
}

func (f *TrackFile) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0444
	// set modest default size 0; real implementation should estimate
	a.Size = 0
	a.Mtime = time.Now()
	return nil
}

// Ensure interfaces are implemented
var _ fs.FS = (*RootDir)(nil)
var _ fs.Node = (*RootDir)(nil)
var _ fs.HandleReadDirAller = (*RootDir)(nil)
var _ fs.NodeStringLookuper = (*RootDir)(nil)
var _ fs.Node = (*AlbumDir)(nil)
var _ fs.HandleReadDirAller = (*AlbumDir)(nil)
var _ fs.NodeStringLookuper = (*AlbumDir)(nil)
var _ fs.Node = (*TrackFile)(nil)
