// Command rillmigrate converts a raw /rill/v1 commit request body into the
// /rill/v2 typed operation envelope used by the new SDK.
//
//	rillmigrate [-in v1.json] [-out v2.json] [-force]
//
// Input defaults to stdin and output to stdout. Exit status: 0 when the
// converted body fits the shared request limit; 3 when it exceeds the limit
// and the original v1 request remains the replay path; 4 when the input is
// not a convertible v1 commit request; 1 on usage or I/O errors.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"rillweave/sdk"
)

func main() {
	in := flag.String("in", "", "raw /rill/v1 commit request body (default stdin)")
	out := flag.String("out", "", "v2 envelope request destination (default stdout)")
	force := flag.Bool("force", false, "write the converted body even when it exceeds the request limit")
	flag.Parse()
	os.Exit(run(*in, *out, *force))
}

func run(inPath, outPath string, force bool) int {
	raw, err := readAll(inPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rillmigrate: %v\n", err)
		return 1
	}
	conversion, err := sdk.Convert(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rillmigrate: %v; replay the input unchanged via POST /rill/v1/graphs/{name}/commit\n", err)
		return 4
	}
	fmt.Fprintf(os.Stderr, "rillmigrate: v1 %d bytes -> v2 %d bytes (request limit %d bytes)\n",
		conversion.V1Bytes, conversion.V2Bytes, conversion.Limit)
	overflow := conversion.Overflow()
	if overflow != nil {
		fmt.Fprintf(os.Stderr, "rillmigrate: %v\n", overflow)
		if !force {
			return 3
		}
		fmt.Fprintln(os.Stderr, "rillmigrate: -force writes the oversized body for limit verification only")
	}
	if err := writeAll(outPath, conversion.Body); err != nil {
		fmt.Fprintf(os.Stderr, "rillmigrate: %v\n", err)
		return 1
	}
	if overflow != nil {
		return 3
	}
	return 0
}

func readAll(path string) ([]byte, error) {
	if path == "" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

func writeAll(path string, body []byte) error {
	if path == "" {
		_, err := os.Stdout.Write(body)
		return err
	}
	return os.WriteFile(path, body, 0600)
}
