package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runBench A/B-tests microfiche against a plain Read through headless
// Claude Code: same file, same question, with and without the server.
func runBench(file, model, profile, question string, reps int) {
	abs, err := filepath.Abs(file)
	if err != nil {
		log.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}

	cfg, err := os.CreateTemp("", "microfiche-bench-*.json")
	if err != nil {
		log.Fatal(err)
	}
	defer os.Remove(cfg.Name())
	json.NewEncoder(cfg).Encode(map[string]any{
		"mcpServers": map[string]any{
			"microfiche": map[string]any{
				"command": exe,
				"args":    []string{"-profile", profile},
			},
		},
	})
	cfg.Close()

	type stats struct {
		freshIn, out, turns int
		cost, secs          float64
		answer              string
	}
	run := func(withMF bool) stats {
		prompt := fmt.Sprintf("Read the file %s and answer.\n%s",
			abs, question)
		args := []string{"-p", "--model", model, "--output-format", "json"}
		if withMF {
			prompt = fmt.Sprintf("Use the microfiche tool (NOT the Read "+
				"tool) on %s, then answer.\n%s", abs, question)
			args = append(args, "--mcp-config", cfg.Name(),
				"--allowedTools", "mcp__microfiche__microfiche,Read")
		}
		cmd := exec.Command("claude", args...)
		cmd.Stdin = strings.NewReader(prompt)
		t0 := time.Now()
		out, err := cmd.Output()
		if err != nil {
			log.Fatalf("claude CLI failed: %v", err)
		}
		var d struct {
			Result   string  `json:"result"`
			Cost     float64 `json:"total_cost_usd"`
			NumTurns int     `json:"num_turns"`
			Usage    struct {
				Input       int `json:"input_tokens"`
				CacheCreate int `json:"cache_creation_input_tokens"`
				Output      int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(out, &d); err != nil {
			log.Fatalf("bad CLI output: %v", err)
		}
		return stats{
			freshIn: d.Usage.Input + d.Usage.CacheCreate,
			out:     d.Usage.Output, turns: d.NumTurns, cost: d.Cost,
			secs: time.Since(t0).Seconds(), answer: d.Result,
		}
	}

	var base, mf []stats
	for i := 0; i < reps; i++ {
		b := run(false)
		m := run(true)
		base, mf = append(base, b), append(mf, m)
		fmt.Printf("rep %d  baseline: in=%d cost=$%.3f t=%.1fs turns=%d | "+
			"microfiche: in=%d cost=$%.3f t=%.1fs turns=%d\n",
			i+1, b.freshIn, b.cost, b.secs, b.turns,
			m.freshIn, m.cost, m.secs, m.turns)
	}

	mean := func(rows []stats, f func(stats) float64) float64 {
		var s float64
		for _, r := range rows {
			s += f(r)
		}
		return s / float64(len(rows))
	}
	row := func(name string, f func(stats) float64, fmtStr string) {
		b, m := mean(base, f), mean(mf, f)
		delta := "-"
		if b != 0 {
			delta = fmt.Sprintf("%+.0f%%", (m-b)/b*100)
		}
		fmt.Printf("%-18s "+fmtStr+"  "+fmtStr+"  %8s\n", name, b, m, delta)
	}
	fmt.Printf("\n%-18s %10s  %10s  %8s\n",
		"metric", "baseline", "microfiche", "delta")
	row("fresh input tok", func(s stats) float64 { return float64(s.freshIn) }, "%10.0f")
	row("output tok", func(s stats) float64 { return float64(s.out) }, "%10.0f")
	row("cost usd", func(s stats) float64 { return s.cost }, "%10.3f")
	row("wall seconds", func(s stats) float64 { return s.secs }, "%10.1f")
	row("turns", func(s stats) float64 { return float64(s.turns) }, "%10.1f")

	fmt.Printf("\nbaseline answer:   %.200s\n", base[0].answer)
	fmt.Printf("microfiche answer: %.200s\n", mf[0].answer)
	fmt.Println("\nCompare the answers yourself — cost savings only count " +
		"if the quality held.")
}
