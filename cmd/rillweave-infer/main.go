// Command rillweave-infer turns externally exported vertex/arc JSON into
// an installable Rillweave graph in two phases:
//
//	rillweave-infer infer   <inputs...> -out <dir> [-name <graph>]
//	rillweave-infer build   <inputs...> -profile <file> -decisions <file> -out <dir>
//	rillweave-infer check   -profile <file>
//
// Inference scans every record once and writes a checksummed profile and
// a starter decisions document. Build reuses those artifacts: naming
// iterations only rerun build, and any input change invalidates the
// profile digest so a stale decision set can never be applied.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"rillweave/assemble"
	"rillweave/image"
	"rillweave/ingest"
	"rillweave/profile"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "rillweave-infer:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("missing subcommand")
	}
	switch args[0] {
	case "infer":
		return inferCmd(args[1:])
	case "build":
		return buildCmd(args[1:])
	case "check":
		return checkCmd(args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  rillweave-infer infer <input-file-or-dir>... -out <dir> [-name graph-name]
  rillweave-infer build <input-file-or-dir>... -profile profile.json -decisions decisions.json -out <dir>
  rillweave-infer check -profile profile.json
`)
}

func loadInputs(patterns []string) ([]ingest.InputFile, error) {
	paths, err := ingest.Expand(patterns)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no input .json files matched")
	}
	files := make([]ingest.InputFile, 0, len(paths))
	for _, path := range paths {
		f, err := ingest.ReadFile(path)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

func inferCmd(args []string) error {
	fs := flag.NewFlagSet("infer", flag.ContinueOnError)
	outDir := fs.String("out", "", "output directory for profile and starter decisions")
	name := fs.String("name", "inferred", "graph name used in the starter decisions")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *outDir == "" || fs.NArg() == 0 {
		usage()
		return fmt.Errorf("infer requires inputs and -out")
	}
	files, err := loadInputs(fs.Args())
	if err != nil {
		return err
	}
	p := profile.Build(files)
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	profilePath := filepath.Join(*outDir, "profile.json")
	if err := writeJSON(profilePath, p); err != nil {
		return err
	}
	starter := assemble.StarterDecisions(p, *name)
	decisionsPath := filepath.Join(*outDir, "decisions.starter.json")
	if err := writeJSON(decisionsPath, starter); err != nil {
		return err
	}
	blockers, conflicts, advisory := assemble.CheckProfile(p)
	fmt.Printf("infer: %d files, %d vertex records, %d arc records\n",
		p.Counts.Files, p.Counts.VertexRecords, p.Counts.ArcRecords)
	fmt.Printf("infer: %d blocker(s), %d conflict(s), %d advisory/advisories\n", blockers, conflicts, advisory)
	fmt.Printf("infer: profile    %s\n", profilePath)
	fmt.Printf("infer: decisions  %s (copy to decisions.json and confirm)\n", decisionsPath)
	fmt.Printf("infer: digest     %s\n", p.Digest)
	if blockers+conflicts > 0 {
		fmt.Println("infer: resolve the findings in the profile before build can pass catalog validation")
	}
	return nil
}

func buildCmd(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	outDir := fs.String("out", "", "output directory for graph, image and report")
	profilePath := fs.String("profile", "", "profile.json produced by infer")
	decisionsPath := fs.String("decisions", "", "confirmed decisions document")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *outDir == "" || *profilePath == "" || *decisionsPath == "" || fs.NArg() == 0 {
		usage()
		return fmt.Errorf("build requires inputs, -profile, -decisions and -out")
	}
	p, err := readProfile(*profilePath)
	if err != nil {
		return err
	}
	d, err := readDecisions(*decisionsPath)
	if err != nil {
		return err
	}
	files, err := loadInputs(fs.Args())
	if err != nil {
		return err
	}
	if err := assemble.VerifyInputs(p, files); err != nil {
		return err
	}
	g, report, err := assemble.Build(p, d, files)
	if err != nil {
		return err
	}
	envelope, err := image.Encode(g)
	if err != nil {
		return fmt.Errorf("image encoding: %w", err)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	graphPath := filepath.Join(*outDir, "graph.json")
	if err := writeJSON(graphPath, g); err != nil {
		return err
	}
	schemaPath := filepath.Join(*outDir, "schema.json")
	if err := writeJSON(schemaPath, g.Schema); err != nil {
		return err
	}
	imagePath := filepath.Join(*outDir, "image.json")
	if err := writeJSON(imagePath, envelope); err != nil {
		return err
	}
	reportPath := filepath.Join(*outDir, "build-report.json")
	if err := writeJSON(reportPath, report); err != nil {
		return err
	}
	fmt.Printf("build: %d vertices, %d arcs; catalog check %s\n",
		report.Included.Vertices, report.Included.Arcs, report.CatalogCheck)
	fmt.Printf("build: graph   %s\n", graphPath)
	fmt.Printf("build: schema  %s\n", schemaPath)
	fmt.Printf("build: image   %s (PUT /rill/v1/graphs/%s/image to install)\n", imagePath, g.Name)
	fmt.Printf("build: report  %s\n", reportPath)
	return nil
}

func checkCmd(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	profilePath := fs.String("profile", "", "profile.json produced by infer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *profilePath == "" {
		usage()
		return fmt.Errorf("check requires -profile")
	}
	p, err := readProfile(*profilePath)
	if err != nil {
		return err
	}
	blockers, conflicts, advisory := assemble.CheckProfile(p)
	for _, f := range p.Findings {
		fmt.Printf("%s %-9s %s  %s\n", f.ID, f.Severity, f.Code, f.Subject)
	}
	fmt.Printf("check: %d blocker(s), %d conflict(s), %d advisory/advisories; digest %s\n",
		blockers, conflicts, advisory, p.Digest)
	if blockers > 0 || conflicts > 0 {
		return fmt.Errorf("profile has unresolved findings")
	}
	return nil
}

func readProfile(path string) (*profile.Profile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p profile.Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}

func readDecisions(path string) (*assemble.Decisions, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d assemble.Decisions
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &d, nil
}

func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o644)
}
