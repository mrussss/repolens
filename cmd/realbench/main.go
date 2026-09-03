package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"repolens/internal/eval/realbench"
)

const defaultDataset = "testdata/realbench/v1"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "validate":
		runValidate(os.Args[2:])
	case "run":
		runBenchmark(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func runValidate(args []string) {
	flags := flag.NewFlagSet("validate", flag.ExitOnError)
	dataRoot := flags.String("data", defaultDataset, "RealBench dataset root")
	_ = flags.Parse(args)

	hash, err := realbench.Validate(*dataRoot)
	if err != nil {
		if hash != "" {
			fmt.Fprintf(os.Stderr, "computed manifest hash: %s\n", hash)
		}
		fatal(err)
	}
	fmt.Printf("valid RealBench dataset: %s\nmanifest hash: %s\n", *dataRoot, hash)
}

func runBenchmark(args []string) {
	flags := flag.NewFlagSet("run", flag.ExitOnError)
	dataRoot := flags.String("data", defaultDataset, "RealBench dataset root")
	cacheRoot := flags.String("cache", ".cache/realbench", "checkout cache root")
	artifactRoot := flags.String("artifacts", "artifacts/realbench", "benchmark artifact root")
	caseID := flags.String("case", "", "run one case, for example REAL-001")
	all := flags.Bool("all", false, "run every case in the manifest")
	e2e := flags.Bool("e2e", false, "run the optional real Agent path when provider configuration is available")
	_ = flags.Parse(args)

	if (*caseID == "") == !*all {
		fatal(fmt.Errorf("choose exactly one of --case REAL-NNN or --all"))
	}
	dataset, err := realbench.LoadInputs(*dataRoot)
	if err != nil {
		fatal(err)
	}
	var caseIDs []string
	if *caseID != "" {
		caseIDs = []string{strings.TrimSpace(*caseID)}
	}
	result, err := realbench.NewRunner(dataset).Run(context.Background(), realbench.RunOptions{
		CaseIDs:      caseIDs,
		CacheDir:     *cacheRoot,
		ArtifactRoot: *artifactRoot,
		RunE2E:       *e2e,
	})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("run: %s\n", result.RunDir)
	fmt.Printf("total=%d completed=%d infra_errors=%d product_failures=%d hit@5=%.3f hit@10=%.3f mrr=%.3f e2e=%s\n",
		result.Metrics.TotalCases, result.Metrics.CompletedCases, result.Metrics.InfraErrors,
		result.Metrics.ProductFailures, result.Metrics.HitAt5, result.Metrics.HitAt10,
		result.Metrics.MRR, result.Metadata.E2EStatus)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: realbench validate [--data testdata/realbench/v1]")
	fmt.Fprintln(os.Stderr, "       realbench run --case REAL-001 [--data ...] [--e2e]")
	fmt.Fprintln(os.Stderr, "       realbench run --all [--data ...] [--e2e]")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "realbench:", err)
	os.Exit(1)
}
