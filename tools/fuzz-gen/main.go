package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// TaskConfig defines the output format for the Arena
type TaskConfig struct {
	Level       string `json:"level"`
	Description string `json:"description"`
	CrashInput  string `json:"crash_input"` // Path to crash file
}

func main() {
	targetFile := flag.String("file", "", "Path to Go file with Fuzz target")
	fuzzTarget := flag.String("target", "", "Name of Fuzz function (e.g. FuzzParse)")
	outDir := flag.String("out", "tasks", "Output directory for the generated task")
	flag.Parse()

	if *targetFile == "" || *fuzzTarget == "" {
		flag.Usage()
		os.Exit(1)
	}

	src, err := os.ReadFile(*targetFile)
	if err != nil {
		fatal(err)
	}

	// Try mutations until one causes a fuzz failure
	maxAttempts := 20
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	fmt.Printf("🎯 Targeting %s in %s\n", *fuzzTarget, *targetFile)

	for i := 0; i < maxAttempts; i++ {
		fmt.Printf("Attempt %d/%d: ", i+1, maxAttempts)
		mutated, desc := mutate(src, rng)
		if bytes.Equal(mutated, src) {
			fmt.Println("No mutation applied, skipping.")
			continue
		}

		// Apply mutation
		backup := *targetFile + ".bak"
		if err := os.WriteFile(backup, src, 0o644); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(*targetFile, mutated, 0o644); err != nil {
			fatal(err)
		}

		// Run Fuzzer
		fmt.Printf("Fuzzing (%s)... ", desc)
		crashFile, _ := runFuzzer(*targetFile, *fuzzTarget)

		// Restore original
		if err := os.WriteFile(*targetFile, src, 0o644); err != nil {
			fatal(err)
		}
		if err := os.Remove(backup); err != nil {
			fatal(err)
		}

		if crashFile != "" {
			fmt.Printf("💥 CRASH FOUND! Saved to %s\n", crashFile)
			saveTask(*outDir, *targetFile, mutated, crashFile, desc)
			return
		} else {
			fmt.Println("No crash found (survived 5s).")
		}
	}
	fmt.Println("❌ Failed to generate a crashing mutation after all attempts.")
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

// mutate applies a random mutation strategy
func mutate(src []byte, rng *rand.Rand) ([]byte, string) {
	str := string(src)
	strategies := []string{"off-by-one", "nil-strip", "bit-flip", "condition-invert"}
	strategy := strategies[rng.Intn(len(strategies))]

	switch strategy {
	case "condition-invert":
		if strings.Contains(str, " < ") {
			return []byte(strings.Replace(str, " < ", " >= ", 1)), "Inverted < to >="
		}
	case "off-by-one":
		// Replace < with <= or vice versa
		if strings.Contains(str, " < ") {
			return []byte(strings.Replace(str, " < ", " <= ", 1)), "Changed < to <="
		}
		if strings.Contains(str, " <= ") {
			return []byte(strings.Replace(str, " <= ", " < ", 1)), "Changed <= to <"
		}
	case "nil-strip":
		// Remove 'if err != nil { return ... }'
		// Crude regex
		re := regexp.MustCompile(`if err != nil \{[^}]+\}`)
		if re.MatchString(str) {
			return []byte(re.ReplaceAllString(str, "// stripped err check")), "Removed error check"
		}
	case "bit-flip":
		// Swap & and |
		if strings.Contains(str, " & ") {
			return []byte(strings.Replace(str, " & ", " | ", 1)), "Changed & to |"
		}
	}
	return src, "none"
}

// runFuzzer runs 'go test -fuzz' and returns path to crash file if found
func runFuzzer(path, target string) (string, error) {
	dir := filepath.Dir(path)
	// Run fuzzing for 5 seconds
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "test", "-fuzz", "^"+target+"$", "-fuzztime", "5s", ".")
	cmd.Dir = dir

	// Capture output to find crash file name
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	output := out.String()

	// Look for "Failing input written to testdata/fuzz/FuzzXxx/..."
	re := regexp.MustCompile(`Failing input written to (testdata/fuzz/[^\s]+)`)
	matches := re.FindStringSubmatch(output)
	if len(matches) > 1 {
		return filepath.Join(dir, matches[1]), nil
	}

	return "", err
}

func saveTask(outDir, originalPath string, mutatedSrc []byte, crashFile, desc string) {
	taskID := fmt.Sprintf("task_%d", time.Now().Unix())
	taskDir := filepath.Join(outDir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		fatal(err)
	}

	// 1. Save Mutated Source
	base := filepath.Base(originalPath) // e.g. parser.go
	if err := os.WriteFile(filepath.Join(taskDir, base), mutatedSrc, 0o644); err != nil {
		fatal(err)
	}

	// 2. Save Crash Input
	crashContent, _ := os.ReadFile(crashFile)
	if err := os.WriteFile(filepath.Join(taskDir, "crash.txt"), crashContent, 0o644); err != nil {
		fatal(err)
	}

	// 3. Save Manifest
	config := TaskConfig{
		Level:       "Hard",
		Description: fmt.Sprintf("The code has been mutated (%s). It crashes on the provided input.", desc),
		CrashInput:  "crash.txt",
	}
	jsonBytes, _ := json.MarshalIndent(config, "", "  ")
	if err := os.WriteFile(filepath.Join(taskDir, "manifest.json"), jsonBytes, 0o644); err != nil {
		fatal(err)
	}

	fmt.Printf("✅ Task generated in %s\n", taskDir)
}
