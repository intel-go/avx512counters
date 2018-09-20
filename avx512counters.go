// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

func main() {
	type step struct {
		name string
		fn   func() error
	}

	var c collector

	steps := []step{
		{"init collector", c.init},
		{"locate test dir", c.locateTestDir},
		{"read extensions", c.readExtensions},
		{"parse command-line args", c.parseFlags},
		{"validate command-line args", c.validateFlags},
		{"prepare work dir", c.prepareWorkDir},
		{"visit work dir", c.visitWorkDir},
		{"collect counters", c.collectCounters},
	}

	for _, s := range steps {
		if err := s.fn(); err != nil {
			log.Fatalf("%s: %v", s.name, err)
		}
	}
}

type collector struct {
	// memArgRE matches any kind of memory operand.
	// Displacement and indexing expressions are optional.
	memArgRE *regexp.Regexp

	// vmemArgRE is almost like memArgRE, but indexing expression is mandatory
	// and index register must be one of the X/Y/Z.
	vmemArgRE *regexp.Regexp

	// testDir is AVX-512 encoder end2end test suite path.
	testDir string

	// availableExt is a set of available testfiles for evaluation.
	availableExt map[string]bool

	// stats is a combined list of collected statistics.
	stats []*iformStats

	// current holds evaluation state which is valid only
	// during single extension evaluation stage.
	current struct {
		extension string
		scanner   testFileScanner
	}

	// Fields below are initialized by command-line arguments (flags).

	extensions    []string
	perfTool      string
	workDir       string
	iformSpanSize uint
	loopCount     uint
	perfRounds    uint
}

func (c *collector) init() error {
	c.memArgRE = regexp.MustCompile(`(?:-?\d+)?\(\w+\)(?:\(\w+\*[1248]\))?`)
	c.vmemArgRE = regexp.MustCompile(`(?:-?\d+)?\(\w+\)\(([XYZ])\d+\*[1248]\)`)
	c.availableExt = make(map[string]bool)
	return nil
}

func (c *collector) locateTestDir() error {
	goroot := runtime.GOROOT()
	// The AVX-512 encoder end2end test suite path is unlikely to change.
	// If it ever does, this should be updated.
	c.testDir = filepath.Join(goroot,
		"src", "cmd", "asm", "internal", "asm", "testdata", "avx512enc")
	if !fileExists(c.testDir) {
		return fmt.Errorf("can't locate AVX-512 testdata: %s doesn't exist", c.testDir)
	}
	return nil
}

func (c *collector) readExtensions() error {
	files, err := ioutil.ReadDir(c.testDir)
	if err != nil {
		return err
	}

	for _, f := range files {
		ext := strings.TrimSuffix(f.Name(), ".s")
		c.availableExt[ext] = true
	}

	return nil
}

func (c *collector) parseFlags() error {
	extensions := flag.String("extensions", "avx512f,avx512dq,avx512cd,avx512bw",
		`comma-separated list of extensions to be evaluated`)
	flag.StringVar(&c.perfTool, "perf", "perf",
		`perf tool binary name. ocperf and other drop-in replacements will do`)
	flag.StringVar(&c.workDir, "workDir", "./avx512counters-workdir",
		`where to put results and the intermediate files`)
	flag.UintVar(&c.iformSpanSize, "iformSpanSize", 100,
		`how many instruction lines form a single iform span. Higher values slow down the collection`)
	flag.UintVar(&c.loopCount, "loopCount", 1*1000*1000,
		`how many times to execute every iform span. Higher values slow down the collection`)
	flag.UintVar(&c.perfRounds, "perfRounds", 1,
		`how many times to re-validate perf results. Higher values slow down the collection`)

	flag.Parse()

	for _, ext := range strings.Split(*extensions, ",") {
		ext = strings.TrimSpace(ext)
		c.extensions = append(c.extensions, ext)
	}

	absWorkDir, err := filepath.Abs(c.workDir)
	if err != nil {
		return fmt.Errorf("expand -workDir: %v", err)
	}
	c.workDir = absWorkDir
	return nil
}

func (c *collector) validateFlags() error {
	for _, ext := range c.extensions {
		if !c.availableExt[ext] {
			return fmt.Errorf("unavailable extension: %q", ext)
		}
	}

	switch {
	case len(c.extensions) == 0:
		return errors.New("expected at least 1 extension name")
	case c.perfTool == "":
		return errors.New("argument -perf can't be empty")
	case c.iformSpanSize == 0:
		return errors.New("argument -iformSpanSize can't be 0")
	case c.loopCount == 0:
		return errors.New("argument -loopCount can't be 0")
	case c.perfRounds == 0:
		return errors.New("argument -perfRounds can't be 0")
	default:
		return nil
	}
}

func (c *collector) prepareWorkDir() error {
	if !fileExists(c.workDir) {
		if err := os.Mkdir(c.workDir, 0700); err != nil {
			return err
		}
	}

	// Always overwrite the main file, just in case.
	mainFile := filepath.Join(c.workDir, "main.go")
	mainFileContents := fmt.Sprintf(`
		// Code generated by avx512counters. DO NOT EDIT.
		package main
		func avx512routine(*[1024]byte)
		func main() {
			var memory [1024]byte
			for i := 0; i < %d; i++ {
				// Fill memory argument with some values.
				for i := range memory {
					memory[i] = byte(i)
				}
				avx512routine(&memory)
			}
		}`, c.loopCount)
	return ioutil.WriteFile(mainFile, []byte(mainFileContents), 0666)
}

func (c *collector) visitWorkDir() error {
	return os.Chdir(c.workDir)
}

func (c *collector) collectCounters() error {
	for _, ext := range c.extensions {
		filename := filepath.Join(c.testDir, ext+".s")

		c.current.extension = ext
		c.current.scanner = testFileScanner{filename: filename}
		if err := c.current.scanner.init(); err != nil {
			log.Printf("skip %s: can't scan test file: %v", ext, err)
			continue
		}

		stats, err := c.evaluateCurrent()
		if err != nil {
			log.Printf("failed %s: %v", ext, err)
			continue
		}

		c.stats = append(c.stats, stats...)
	}

	return nil
}

func (c *collector) evaluateCurrent() ([]*iformStats, error) {
	return nil, nil
}

// iformStats is parsed perf output for particular instruction forme evaluation.
type iformStats struct {
	ext   string // extension instruction belongs to
	iform string // tested instruction form

	level0 int64 // turbo0 event counter
	level1 int64 // turbo1 event counter
	level2 int64 // turbo2 event counter
}

// testLine is decoded end2end test file line.
// Represents single instruction.
type testLine struct {
	op   string   // Opcode
	args []string // Instruction arguments
	text string   // Asm text (whole line)
}

// testFileScanner reads Go asm end2end test file into testLine objects.
type testFileScanner struct {
	filename string
	lineRE   *regexp.Regexp
	lines    []string // Unprocessed lines
	line     testLine // Last decoded line
	err      error
}

func (s *testFileScanner) init() error {
	data, err := ioutil.ReadFile(s.filename)
	if err != nil {
		return err
	}
	s.lineRE = regexp.MustCompile(`\t(.*?) (.*?) // [0-9a-f]+`)
	s.lines = strings.Split(string(data), "\n")
	// Instructions lines start with "\t".
	// Skip everything before them.
	for len(s.lines[0]) == 0 || s.lines[0][0] != '\t' {
		s.lines = s.lines[1:]
	}
	return nil
}

func (s *testFileScanner) scan() bool {
	if s.err != nil {
		return false
	}
	if len(s.lines) == 0 {
		s.err = fmt.Errorf("%s: unexpected EOF (expected RET instruction)", s.filename)
		return false
	}
	if s.lines[0] == "\tRET" {
		return false
	}
	m := s.lineRE.FindStringSubmatch(s.lines[0])
	if m == nil {
		s.err = fmt.Errorf("%s: unexpected %q line (does not match pattern)", s.filename, s.lines[0])
		return false
	}
	s.lines = s.lines[1:]
	var args []string
	for _, x := range strings.Split(m[2], ",") {
		args = append(args, strings.TrimSpace(x))
	}
	s.line = testLine{op: m[1], args: args, text: m[0]}
	return true
}

// fileExists reports whether file with given name exists.
func fileExists(name string) bool {
	_, err := os.Stat(name)
	return !os.IsNotExist(err)
}
