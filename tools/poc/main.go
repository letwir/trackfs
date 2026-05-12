package main

import (
"bufio"
"encoding/json"
"fmt"
"os"
"os/exec"
"path/filepath"
"regexp"
"strconv"
"strings"
)

type Track struct {
Number      int     `json:"number"`
Title       string  `json:"title"`
Index       string  `json:"index"`
StartSecond float64 `json:"start_second"`
}

func main() {
if len(os.Args) < 2 {
fmt.Fprintln(os.Stderr, "usage: poc-metaflac <file.flac>")
os.Exit(2)
}
file := os.Args[1]

cueText, err := getCueFromMetaflac(file)
if err != nil || strings.TrimSpace(cueText) == "" {
// fallback to .cue file
base := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
cuePath := filepath.Join(filepath.Dir(file), base+".cue")
b, rerr := os.ReadFile(cuePath)
if rerr != nil {
fmt.Fprintln(os.Stderr, "no CUESHEET found via metaflac nor .cue file")
os.Exit(1)
}
cueText = string(b)
}

tracks := parseCue(cueText)
if len(tracks) == 0 {
    fmt.Fprintln(os.Stderr, "[DEBUG] parsed 0 tracks; showing CUESHEET head:")
    head := cueText
    if len(head) > 2000 {
        head = head[:2000]
    }
    fmt.Fprintln(os.Stderr, head)
}
if tracks == nil {
    tracks = make([]Track, 0)
}
enc := json.NewEncoder(os.Stdout)
enc.SetIndent("", "  ")
_ = enc.Encode(tracks)
}

func getCueFromMetaflac(path string) (string, error) {
cmd := exec.Command("metaflac", "--show-tag=CUESHEET", path)
out, err := cmd.Output()
if err != nil {
return "", err
}
return string(out), nil
}

func parseCue(r string) []Track {
s := bufio.NewScanner(strings.NewReader(r))
trackRe := regexp.MustCompile(`^\\s*TRACK\\s+(\\d+)`) 
titleRe := regexp.MustCompile(`^\\s*TITLE\\s+"(.*)"`) 
indexRe := regexp.MustCompile(`^\\s*INDEX\\s+01\\s+(\\d{2}:\\d{2}:\\d{2})`)

tracks := make([]Track, 0)
var cur Track
inTrack := false
for s.Scan() {
line := s.Text()
if m := trackRe.FindStringSubmatch(line); m != nil {
if inTrack {
tracks = append(tracks, cur)
}
inTrack = true
num, _ := strconv.Atoi(m[1])
cur = Track{Number: num}
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
cur.Index = m[1]
cur.StartSecond = parseIndexToSeconds(m[1])
continue
}
}
if inTrack {
tracks = append(tracks, cur)
}
return tracks
}

func parseIndexToSeconds(idx string) float64 {
parts := strings.Split(idx, ":")
if len(parts) != 3 {
return 0
}
mm, _ := strconv.Atoi(parts[0])
ss, _ := strconv.Atoi(parts[1])
ff, _ := strconv.Atoi(parts[2])
// cue frames are 75 frames per second
return float64(mm*60+ss) + float64(ff)/75.0
}
