package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type TrackEntry struct {
	VPath string `json:"vpath"`
	Num   int    `json:"num"`
	Title string `json:"title"`
	Source string `json:"source"`
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: poc-manifest <file.flac>\n")
	}
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	src := flag.Arg(0)
	tracks := getTracks(src)
	entries := make([]TrackEntry, 0, len(tracks))
	base := filepath.Base(src)
	root := filepath.Dir(src)
	albumName := base[:len(base)-len(filepath.Ext(base))]
	for _, t := range tracks {
		v := filepath.Join(root, albumName, fmt.Sprintf("%03d.%s.flac", t.Number, sanitizeForFs(t.Title)))
		entries = append(entries, TrackEntry{VPath: v, Num: t.Number, Title: t.Title, Source: src})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(entries)
}

func sanitizeForFs(s string) string {
	// crude: replace path separators and leading/trailing spaces
	s = filepath.Clean(s)
	s = stringReplaceAll(s, string(os.PathSeparator), "_")
	return s
}

func stringReplaceAll(s, old, new string) string {
	return strings.ReplaceAll(s, old, new)
}

// getTracks uses the existing poc-metaflac parsing

func getTracks(src string) []struct{Number int; Title string} {
	cmdPath := "./poc-metaflac"
	if _, err := os.Stat(cmdPath); err != nil {
		return []struct{Number int; Title string}{}
	}
	cmd := exec.Command(cmdPath, src)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return []struct{Number int; Title string}{}
	}
	var parsed []struct{
		Number int    `json:"number"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		return []struct{Number int; Title string}{}
	}
	res := make([]struct{Number int; Title string}, len(parsed))
	for i, p := range parsed {
		res[i].Number = p.Number
		res[i].Title = p.Title
	}
	return res
}

