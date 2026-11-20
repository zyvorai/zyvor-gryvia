// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// verifyobj loads every eBPF object file in a directory through the kernel verifier without attaching anything.
// It is the CI check for ebpf/ (no bpftool needed) and needs root and a kernel with BTF:
//
//	sudo go run ./cmd/verifyobj ../ebpf
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
)

func main() {
	dir := "."
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.o"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no .o files in %s\n", dir)
		os.Exit(2)
	}
	sort.Strings(files)
	if err := rlimit.RemoveMemlock(); err != nil {
		fmt.Fprintf(os.Stderr, "cannot raise the memlock limit: %v\n", err)
		os.Exit(2)
	}

	failed := 0
	for _, f := range files {
		name := filepath.Base(f)
		if err := load(f); err != nil {
			failed++
			fmt.Printf("LOAD FAIL %s: %v\n", name, err)
			continue
		}
		fmt.Printf("LOAD OK   %s\n", name)
	}
	fmt.Printf("%d objects, %d failed\n", len(files), failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func load(path string) error {
	spec, err := ebpf.LoadCollectionSpec(path)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	coll, err := ebpf.NewCollectionWithOptions(spec, ebpf.CollectionOptions{
		Programs: ebpf.ProgramOptions{LogSizeStart: 16 << 20},
	})
	if err != nil {
		var ve *ebpf.VerifierError
		if errors.As(err, &ve) {
			return fmt.Errorf("verifier: %+v", ve)
		}
		return err
	}
	coll.Close()
	return nil
}
