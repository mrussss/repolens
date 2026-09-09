package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"repolens/internal/eval/realbench"
)

const defaultDatasetVersion = "v1"

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
	datasetVersion := flags.String("dataset", defaultDatasetVersion, "RealBench dataset version: v1 or v2")
	dataRoot := flags.String("data", "", "RealBench dataset root (overrides --dataset)")
	_ = flags.Parse(args)
	root, err := resolveDatasetRoot(*datasetVersion, *dataRoot)
	if err != nil {
		fatal(err)
	}

	hash, err := realbench.Validate(root)
	if err != nil {
		if hash != "" {
			fmt.Fprintf(os.Stderr, "computed manifest hash: %s\n", hash)
		}
		fatal(err)
	}
	fmt.Printf("valid RealBench dataset: %s\nmanifest hash: %s\n", root, hash)
}

func runBenchmark(args []string) {
	flags := flag.NewFlagSet("run", flag.ExitOnError)
	datasetVersion := flags.String("dataset", defaultDatasetVersion, "RealBench dataset version: v1 or v2")
	dataRoot := flags.String("data", "", "RealBench dataset root (overrides --dataset)")
	cacheRoot := flags.String("cache", ".cache/realbench", "checkout cache root")
	artifactRoot := flags.String("artifacts", "artifacts/realbench", "benchmark artifact root")
	caseID := flags.String("case", "", "run one case, for example REAL-001")
	all := flags.Bool("all", false, "run every case in the manifest")
	e2e := flags.Bool("e2e", false, "run the optional real Agent path when provider configuration is available")
	_ = flags.Parse(args)

	if (*caseID == "") == !*all {
		fatal(fmt.Errorf("choose exactly one of --case REAL-NNN or --all"))
	}
	root, err := resolveDatasetRoot(*datasetVersion, *dataRoot)
	if err != nil {
		fatal(err)
	}
	dataset, err := realbench.LoadInputs(root)
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
	fmt.Fprintln(os.Stderr, "usage: realbench validate [--dataset v1|v2] [--data ...]")
	fmt.Fprintln(os.Stderr, "       realbench run --dataset v1|v2 --case REAL-NNN [--data ...] [--e2e]")
	fmt.Fprintln(os.Stderr, "       realbench run --dataset v1|v2 --all [--data ...] [--e2e]")
}

func resolveDatasetRoot(version, explicitRoot string) (string, error) {
	if strings.TrimSpace(explicitRoot) != "" {
		return explicitRoot, nil
	}
	switch strings.ToLower(strings.TrimSpace(version)) {
	case "v1", "1", "realbench-v1":
		return filepath.Join("testdata", "realbench", "v1"), nil
	case "v2", "2", "realbench-v2":
		return filepath.Join("testdata", "realbench", "v2"), nil
	default:
		return "", fmt.Errorf("unsupported RealBench dataset %q; choose v1 or v2", version)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "realbench:", err)
	os.Exit(1)
}
