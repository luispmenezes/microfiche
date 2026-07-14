package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// printStats summarizes ~/.microfiche/log.jsonl: what was imaged, what was
// skipped and why, and the estimated token/cost savings.
func printStats() {
	path := filepath.Join(homeDir(), ".microfiche", "log.jsonl")
	f, err := os.Open(path)
	if err != nil {
		fmt.Printf("no telemetry yet (%s)\n", path)
		return
	}
	defer f.Close()

	var imaged, bailedSmall, bailedSparse, skippedLarge int
	var textTok, imgTok, savedTok float64
	perFile := map[string]float64{}
	var first, last float64

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e map[string]any
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if ts, ok := e["ts"].(float64); ok {
			if first == 0 {
				first = ts
			}
			last = ts
		}
		switch {
		case e["skipped"] == true:
			skippedLarge++
		case e["bailed"] == true:
			if e["reason"] == "sparse" {
				bailedSparse++
			} else {
				bailedSmall++
			}
		default:
			imaged++
			t, _ := e["est_text_tokens"].(float64)
			i, _ := e["est_image_tokens"].(float64)
			textTok += t
			imgTok += i
			savedTok += t - i
			if name, ok := e["file"].(string); ok {
				perFile[name] += t - i
			}
		}
	}

	span := ""
	if first > 0 {
		span = fmt.Sprintf(" (%s — %s)",
			time.Unix(int64(first), 0).Format("2006-01-02"),
			time.Unix(int64(last), 0).Format("2006-01-02"))
	}
	fmt.Printf("microfiche stats%s\n\n", span)
	fmt.Printf("  imaged calls:        %d\n", imaged)
	fmt.Printf("  bailed (too small):  %d\n", bailedSmall)
	fmt.Printf("  bailed (sparse):     %d\n", bailedSparse)
	fmt.Printf("  skipped (too large): %d\n", skippedLarge)
	if imaged > 0 {
		fmt.Printf("\n  text tokens replaced:  %.0f\n", textTok)
		fmt.Printf("  image tokens delivered: %.0f\n", imgTok)
		fmt.Printf("  est tokens saved:      %.0f (%.1fx compression)\n",
			savedTok, textTok/imgTok)
		fmt.Printf("  est input cost saved:  $%.2f (at $10/MTok input)\n",
			savedTok/1e6*10)
	}
	if len(perFile) > 0 {
		type kv struct {
			k string
			v float64
		}
		var top []kv
		for k, v := range perFile {
			top = append(top, kv{k, v})
		}
		sort.Slice(top, func(a, b int) bool {
			return top[a].v > top[b].v
		})
		fmt.Printf("\n  top files by savings:\n")
		for i, e := range top {
			if i >= 5 {
				break
			}
			fmt.Printf("    %8.0f tok  %s\n", e.v, e.k)
		}
	}
}
