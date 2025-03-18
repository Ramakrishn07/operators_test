package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/google/go-github/v53/github"
	"golang.org/x/oauth2"
)

func main() {
	repoRange := flag.String("range", "", "Range of repos to test (e.g. 1-10). Empty means test all.")
	flag.Parse()

	reportFile, err := os.Create("test_report.txt")
	if err != nil {
		log.Fatalf("Failed to create report file: %v", err)
	}
	defer reportFile.Close()
	writer := bufio.NewWriter(reportFile)

	repositories, err := fetchOperatorRepos()
	if err != nil {
		log.Fatalf("Failed to fetch operator repos: %v", err)
	}

	if len(repositories) == 0 {
		log.Println("No operator repositories found.")
		return
	}

	sort.Strings(repositories)

	fmt.Println("Found", len(repositories), "operator repos:")
	for i, repoURL := range repositories {
		fmt.Printf("%3d) %s\n", i+1, repoURL)
	}

	startIndex, endIndex, err := parseRange(*repoRange, len(repositories))
	if err != nil {
		log.Fatalf("Error parsing --range=%q: %v", *repoRange, err)
	}

	var chosenRepos []string
	if startIndex == 0 && endIndex == 0 {
		chosenRepos = repositories
		fmt.Println("\nNo range specified; testing ALL repos.\n")
	} else {
		chosenRepos = repositories[startIndex-1 : endIndex]
		fmt.Printf("\nTesting repos from %d to %d (inclusive)\n", startIndex, endIndex)
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, 5) //  concurrency

	baseDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get current working directory: %v", err)
	}

	for _, repoURL := range chosenRepos {
		repoURL := repoURL
		wg.Add(1)
		go func(repoURL string) {
			defer wg.Done()
			sem <- struct{}{} // Acquire a slot
			defer func() { <-sem }() // Release the slot

			repoName := getRepoName(repoURL)
			fmt.Println("Cloning repository:", repoURL)

			cmd := exec.Command("git", "clone", "--depth=1", repoURL)
			if err := cmd.Run(); err != nil {
				fmt.Println("Repository not found or failed to clone:", repoURL)
				_, _ = writer.WriteString(fmt.Sprintf("\n%s\nRepository Not Found.\n", repoName))
				writer.Flush()
				return
			}

			repoPath := fmt.Sprintf("%s/%s", baseDir, repoName)
			if err := os.Chdir(repoPath); err != nil {
				fmt.Println("Failed to cd into repo:", repoName)
				return
			}

			output, _ := runGinkgoTests()

			_ = os.Chdir(baseDir)

			testSummary := processGinkgoOutput(output)

			_, err := writer.WriteString(fmt.Sprintf("\n%s\n%s\n", repoName, testSummary))
			if err != nil {
				fmt.Println("Error writing to report file:", err)
			}
			writer.Flush()
		}(repoURL)
	}

	wg.Wait()
	fmt.Println("\nTest execution completed. Results saved in test_report.txt")
}

func fetchOperatorRepos() ([]string, error) {
	ghToken := os.Getenv("GITHUB_TOKEN")
	if ghToken == "" {
		return nil, fmt.Errorf("no GITHUB_TOKEN set")
	}

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

func runGinkgoTests() (string, error) {
	cmd := exec.Command("ginkgo", "--flake-attempts=3", "--tags=osde2e", "-vv", "--trace", "./...")
	outputBytes, err := cmd.CombinedOutput()
	output := string(outputBytes)

	if err != nil && strings.Contains(output, "Ginkgo detected a version mismatch") {
		altCmd := exec.Command("go", "run", "github.com/onsi/ginkgo/v2/ginkgo",
			"--flake-attempts=3", "--tags=osde2e", "-vv", "--trace", "./...")
		outputBytes, err = altCmd.CombinedOutput()
		output = string(outputBytes)
	}

	return output, err
}

func processGinkgoOutput(output string) string {
	if strings.Contains(output, "Summarizing ") {
		var resultLines []string
		lines := strings.Split(output, "\n")
		inSummaries := false
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "Summarizing ") {
				inSummaries = true
				continue
			}
			if inSummaries {
				if trimmed == "" || strings.HasPrefix(trimmed, "Ran ") {
					inSummaries = false
					break
				}
				if strings.HasPrefix(trimmed, "[FAIL]") || strings.HasPrefix(trimmed, "[FLAKE]") {
					resultLines = append(resultLines, trimmed)
				}
			}
		}
		if len(resultLines) > 0 {
			return fmt.Sprintf("Failures/Flakes:\n%s\n", strings.Join(resultLines, "\n"))
		}
	}

	failureRegex := regexp.MustCompile(`There were failures detected in the following suites:\s+(.+)`)
	failureMatches := failureRegex.FindStringSubmatch(output)
	if len(failureMatches) > 1 {
		failingSuite := strings.TrimSpace(failureMatches[1])
		return fmt.Sprintf("Failing test suite: %s\n", failingSuite)
	}

	return "No failing or flaky tests detected.\n"
}

func parseRange(r string, max int) (int, int, error) {
	if r == "" {
		return 0, 0, nil
	}
	parts := strings.Split(r, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("range must be in the form start-end (e.g. 1-10)")
	}
	start, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid start: %v", err)
	}
	end, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid end: %v", err)
	}
	if start < 1 || end < 1 || start > max || end > max || start > end {
		return 0, 0, fmt.Errorf("range %d-%d is out of valid bounds (1-%d)", start, end, max)
	}
	return start, end, nil
}

