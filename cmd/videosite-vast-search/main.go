// videosite-vast-search runs the same Vast.ai offer search the encoder
// orchestrator uses, so you can diagnose "no offers matched" failures
// without firing a real encoding job. Filter flags default to the
// production values; pass 0 / empty to drop the corresponding filter
// when bisecting which one is killing your results.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/facebookgo/flagenv"
	_ "github.com/joho/godotenv/autoload"

	"github.com/tigrisdata-community/videosite/internal/encoder"
	xslog "github.com/tigrisdata-community/videosite/internal/slog"
)

var (
	vastAPIKey     = flag.String("vast-api-key", "", "Vast.ai API key")
	vastAPIBase    = flag.String("vast-api-base", "https://console.vast.ai", "Vast.ai API base URL")
	gpuPrefs       = flag.String("gpu-prefs", strings.Join(encoder.DefaultGPUPrefs, ","), "comma-separated GPU names, highest priority first; empty = no gpu_name filter")
	minReliability = flag.Float64("min-reliability", encoder.DefaultMinReliability, "minimum host reliability; 0 = no filter")
	minCUDA        = flag.Float64("min-cuda", 12.0, "minimum cuda_max_good; 0 = no filter")
	minInetDown    = flag.Int("min-inet-down", 200, "minimum inet_down Mbps; 0 = no filter")
	numGPUs        = flag.Int("num-gpus", 1, "require exactly this many GPUs; 0 = no filter")
	limit          = flag.Int("limit", 20, "max offers to return")
	asJSON         = flag.Bool("json", false, "emit raw offer list as JSON")
	verbose        = flag.Bool("verbose", false, "print the request query and raw response body")
)

func main() {
	flagenv.Parse()
	flag.Parse()
	xslog.Init()

	if *vastAPIKey == "" {
		slog.Error("vast-api-key is required (set VAST_API_KEY or pass --vast-api-key)")
		os.Exit(2)
	}

	var prefs []string
	if *gpuPrefs != "" {
		for p := range strings.SplitSeq(*gpuPrefs, ",") {
			if p = strings.TrimSpace(p); p != "" {
				prefs = append(prefs, p)
			}
		}
	}

	query := buildQuery(prefs)

	if *verbose {
		dumpJSON(os.Stderr, "query", query)
	}

	ctx := context.Background()
	c := encoder.NewVastClient(*vastAPIKey, *vastAPIBase, nil)
	offers, raw, err := c.SearchOffersRaw(ctx, query)
	if err != nil {
		slog.Error("search offers", "err", err)
		os.Exit(1)
	}
	if *verbose {
		fmt.Fprintln(os.Stderr, "raw response:")
		fmt.Fprintln(os.Stderr, string(raw))
	}

	if *asJSON {
		out := struct {
			Query  map[string]any  `json:"query"`
			Offers []encoder.Offer `json:"offers"`
			Picked *encoder.Offer  `json:"picked,omitempty"`
		}{Query: query, Offers: offers}
		if picked, perr := encoder.PickOffer(offers, prefs); perr == nil {
			out.Picked = &picked
		}
		dumpJSON(os.Stdout, "", out)
		return
	}

	fmt.Printf("found %d offers\n\n", len(offers))
	if len(offers) == 0 {
		fmt.Println("no offers matched; try lowering --min-reliability/--min-cuda/--min-inet-down,")
		fmt.Println("setting --num-gpus 0, or expanding --gpu-prefs. Re-run with --verbose to see")
		fmt.Println("the exact query and raw API response.")
		os.Exit(1)
	}

	fmt.Printf("%-8s  %-12s  %-3s  %-8s  %-11s  %s\n",
		"ASK_ID", "GPU", "N", "VRAM_GB", "RELIABILITY", "$/HR")
	for _, o := range offers {
		fmt.Printf("%-8d  %-12s  %-3d  %-8.1f  %-11.4f  %.4f\n",
			o.AskContractID, o.GpuName, o.NumGpus, o.GpuRAM, o.Reliability2, o.DphTotal)
	}

	if len(prefs) == 0 {
		return
	}
	picked, err := encoder.PickOffer(offers, prefs)
	if err != nil {
		slog.Error("pick offer", "err", err)
		os.Exit(1)
	}
	fmt.Printf("\nwould pick: ask_id=%d gpu=%s dph=%.4f\n",
		picked.AskContractID, picked.GpuName, picked.DphTotal)
}

// buildQuery starts from the production query and applies overrides /
// removals based on CLI flags. A zero or empty flag drops the
// corresponding filter so you can isolate which one is excluding hosts.
func buildQuery(prefs []string) map[string]any {
	q := encoder.PreferredOfferQuery(prefs, *minReliability)
	if *minReliability == 0 {
		delete(q, "reliability")
	}
	if len(prefs) == 0 {
		delete(q, "gpu_name")
	}
	if *minCUDA > 0 {
		q["cuda_max_good"] = map[string]any{"gte": *minCUDA}
	} else {
		delete(q, "cuda_max_good")
	}
	if *minInetDown > 0 {
		q["inet_down"] = map[string]any{"gte": *minInetDown}
	} else {
		delete(q, "inet_down")
	}
	if *numGPUs > 0 {
		q["num_gpus"] = map[string]any{"eq": *numGPUs}
	} else {
		delete(q, "num_gpus")
	}
	if *limit > 0 {
		q["limit"] = *limit
	}
	return q
}

func dumpJSON(w *os.File, label string, v any) {
	if label != "" {
		fmt.Fprintln(w, label+":")
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		slog.Error("encode json", "err", err)
	}
}
