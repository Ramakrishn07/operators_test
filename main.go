// parallel_ginkgo_runner.go
// Dockerfile-compatible test-runner entry point
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v53/github"
	"golang.org/x/oauth2"
)

const (
	skipRepoName = "cluster-kube-apiserver-operator"
	testTimeout  = 3 * time.Minute
)

var (
	failLineRegex = regexp.MustCompile(`\[FAIL\]`)
	flakyRegex    = regexp.MustCompile(`\[FLAKY\]`)
	limit         = flag.Int("limit", 5, "Maximum number of concurrent test executions")
)

func main() {
	selectedRepo := flag.String("repo", "", "Specify a repository name to run tests on (e.g., 'cloud-ingress-operator')")
	flag.Parse()

	ghToken := os.Getenv("GITHUB_TOKEN")
	if ghToken == "" {
		log.Fatal("Error: GITHUB_TOKEN is not set. Exiting.")
	}

	var repositories []string
	var err error

	stdinInfo, _ := os.Stdin.Stat()
	if (stdinInfo.Mode() & os.ModeCharDevice) == 0 {
		repositories, err = readReposFromStdin()
		if err != nil {
			log.Fatalf("Failed to read repos from stdin: %v", err)
		}
	} else if *selectedRepo != "" {
		repoURL := fmt.Sprintf("https://github.com/openshift/%s.git", *selectedRepo)
		repositories = []string{repoURL}
	} else {
		repositories, err = fetchOperatorRepos()
		if err != nil {
			log.Fatalf("Failed to fetch operator repos: %v", err)
		}
	}

	if len(repositories) == 0 {
		log.Println("No operator repositories found.")
		return
	}

	sort.Strings(repositories)
	fmt.Println("Found", len(repositories), "operator repos:")

	reportFile, err := os.Create("test_report.txt")
	if err != nil {
		log.Fatalf("Failed to create report file: %v", err)
	}
	defer reportFile.Close()
	writer := bufio.NewWriter(reportFile)

	skippedFile, err := os.Create("skipped_repos.txt")
	if err != nil {
		log.Fatalf("Failed to create skipped repos file: %v", err)
	}
	defer skippedFile.Close()
	skippedWriter := bufio.NewWriter(skippedFile)

	reposFolder, _ := os.MkdirTemp("", "repos")
	fmt.Println("Cloning repos to:", reposFolder)

	sem := make(chan struct{}, *limit)
	var wg sync.WaitGroup

	for _, repoURL := range repositories {
		repoURL := repoURL
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			processRepo(repoURL, reposFolder, writer, skippedWriter)
		}()
	}

	wg.Wait()
	writer.Flush()
	skippedWriter.Flush()

	fmt.Println("\nTest execution completed. Results saved in test_report.txt")
	fmt.Println("Skipped repos saved in skipped_repos.txt")
}

func readReposFromStdin() ([]string, error) {
	scanner := bufio.NewScanner(os.Stdin)
	var repos []string
	for scanner.Scan() {
		repo := strings.TrimSpace(scanner.Text())
		if repo != "" {
			repos = append(repos, fmt.Sprintf("https://github.com/openshift/%s.git", repo))
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return nil, err
	}
	return repos, nil
}

func processRepo(repoURL, reposFolder string, writer, skippedWriter *bufio.Writer) {
	repoName := getRepoName(repoURL)
	repoPath := filepath.Join(reposFolder, repoName)

	if repoName == skipRepoName {
		fmt.Printf("Skipping repository: %s\n", repoName)
		writer.WriteString(fmt.Sprintf("\n%s\nRepository skipped by policy.\n", repoName))
		writer.Flush()
		return
	}

	fmt.Println("Cloning repository:", repoURL)
	cmd := exec.Command("git", "clone", "--depth=1", repoURL, repoPath)
	if err := cmd.Run(); err != nil {
		fmt.Println("Repository not found or failed to clone:", repoURL)
		writer.WriteString(fmt.Sprintf("\n%s\nRepository Not Found.\n", repoName))
		writer.Flush()
		return
	}
	testDir, err := getTestExecutionDir(repoPath)
	if err != nil {
		fmt.Println("Skipping repo (no valid e2e test directory found):", repoName)
		skippedWriter.WriteString(fmt.Sprintf("%s\n", repoName))
		skippedWriter.Flush()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	done := make(chan struct{})

	var failedTests []string
	var flakyTests []string
	var criticalError string

	go func() {
		uniqueFailures := make(map[string]bool)
		uniqueFlaky := make(map[string]bool)
		for i := 0; i < 3; i++ {
			fmt.Printf("Running test for %s (Attempt %d/3) in directory %s\n", repoName, i+1, testDir)
			output, err := runGinkgoTests(testDir)
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					switch exitErr.ExitCode() {
					case 2:
						criticalError = fmt.Sprintf("[COMPILATION ERROR] %s", output)
					case 3:
						criticalError = fmt.Sprintf("[SETUP ERROR] %s", output)
					}
				}
				if criticalError != "" {
					break
				}
			}
			failed, flaky := parseTestResults(output)
			for _, line := range failed {
				if !uniqueFailures[line] {
					failedTests = append(failedTests, line)
					uniqueFailures[line] = true
				}
			}
			for _, line := range flaky {
				if !uniqueFlaky[line] {
					flakyTests = append(flakyTests, line)
					uniqueFlaky[line] = true
				}
			}
		}
		done <- struct{}{}
	}()

	select {
	case <-ctx.Done():
		msg := "Test suite took too long (>3 minutes), skipping remaining attempts."
		writer.WriteString(fmt.Sprintf("\n%s\n%s\n", repoName, msg))
		writer.Flush()
	case <-done:
		testSummary := generateSummary(failedTests, flakyTests, criticalError)
		writer.WriteString(fmt.Sprintf("\n%s\n%s\n", repoName, testSummary))
		writer.Flush()
	}
}

func fetchOperatorRepos() ([]string, error) {
	ghToken := os.Getenv("GITHUB_TOKEN")
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: ghToken})
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	org := "openshift"
	opt := &github.RepositoryListByOrgOptions{ListOptions: github.ListOptions{PerPage: 100}}
	var allRepos []string
	for {
		repos, resp, err := client.Repositories.ListByOrg(ctx, org, opt)
		if err != nil {
			return nil, fmt.Errorf("error listing repos: %v", err)
		}
		for _, r := range repos {
			name := r.GetName()
			if strings.Contains(strings.ToLower(name), "operator") {
				cloneURL := fmt.Sprintf("https://github.com/%s/%s.git", org, name)
				allRepos = append(allRepos, cloneURL)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return allRepos, nil
}

func getRepoName(repoURL string) string {
	parts := strings.Split(repoURL, "/")
	return strings.TrimSuffix(parts[len(parts)-1], ".git")
}

func getTestExecutionDir(repoPath string) (string, error) {
	e2eFolder := filepath.Join(repoPath, "test", "e2e")
	info, err := os.Stat(e2eFolder)
	if os.IsNotExist(err) || !info.IsDir() {
		return "", fmt.Errorf("e2e folder not found in %s", repoPath)
	}
	files, err := os.ReadDir(e2eFolder)
	if err != nil {
		return "", fmt.Errorf("error reading e2e folder in %s", repoPath)
	}
	hasGoFile := false
	for _, f := range files {
		if !f.IsDir() && filepath.Ext(f.Name()) == ".go" {
			hasGoFile = true
			break
		}
	}
	if !hasGoFile {
		return "", fmt.Errorf("no .go files found in %s", e2eFolder)
	}
	return e2eFolder, nil
}

func runGinkgoTests(testDir string) (string, error) {
	cmd := exec.Command("ginkgo", "-p", "-nodes=4", "--flake-attempts=3", "--tags=e2e,osde2e", "--no-color", "-v", "--trace", ".")
	cmd.Dir = testDir
	outputBytes, err := cmd.CombinedOutput()
	return string(outputBytes), err
}

func parseTestResults(output string) ([]string, []string) {
	var failed, flaky []string
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case failLineRegex.MatchString(line):
			failed = append(failed, line)
		case flakyRegex.MatchString(line):
			flaky = append(flaky, line)
		}
	}
	return failed, flaky
}

func generateSummary(failed, flaky []string, criticalError string) string {
	var summary strings.Builder
	if criticalError != "" {
		summary.WriteString(fmt.Sprintf("Critical Error:\n  - %s\n", criticalError))
		return summary.String()
	}
	if len(failed) > 0 {
		summary.WriteString("Failing Tests:\n")
		for _, line := range failed {
			summary.WriteString(fmt.Sprintf("  - %s\n", line))
		}
	}
	if len(flaky) > 0 {
		summary.WriteString("\nFlaky Tests:\n")
		for _, line := range flaky {
			summary.WriteString(fmt.Sprintf("  - %s\n", line))
		}
	}
	if summary.Len() == 0 {
		return "No failing or flaky tests detected."
	}
	return summary.String()
}
