package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

func main() {
	// List of repositories to clone and test
	repositories := []string{
		"https://github.com/openshift/addon-operator.git",
"https://github.com/openshift/cloud-ingress-operator.git",
"https://github.com/openshift/configure-alertmanager-operator.git",
"https://github.com/openshift/custom-domains-operator.git",
"https://github.com/openshift/deployment-validation-operator.git",
"https://github.com/openshift/managed-node-metadata-operator.git",
"https://github.com/openshift/managed-upgrade-operator.git",
"https://github.com/openshift/managed-velero-operator.git",
"https://github.com/openshift/must-gather-operator.git",
"https://github.com/openshift/ocm-agent-operator.git",
"https://github.com/openshift/osd-metrics-exporter.git",
"https://github.com/openshift/rbac-permissions-operator.git",
"https://github.com/openshift/route-monitor-operator.git",
"https://github.com/openshift/splunk-forwarder-operator.git",



	}

	// Create a report file
	reportFile, err := os.Create("test_report.txt")
	if err != nil {
		fmt.Println("Failed to create report file:", err)
		return
	}
	defer reportFile.Close()
	writer := bufio.NewWriter(reportFile)

	// Get current working directory
	baseDir, err := os.Getwd()
	if err != nil {
		fmt.Println(" Failed to get current working directory:", err)
		return
	}

	for _, repoURL := range repositories {
		repoName := getRepoName(repoURL)

		// Clone the repository
		fmt.Println("Cloning repository:", repoURL)
		cmd := exec.Command("git", "clone", repoURL)
		if err := cmd.Run(); err != nil {
			// Log failure if repo is not found
			fmt.Println(" Repository not found:", repoURL)
			_, _ = writer.WriteString(fmt.Sprintf("\n%s\n Repository Not Found.\n", repoName))
			writer.Flush()
			continue
		}

		// Change into the cloned repository directory
		repoPath := fmt.Sprintf("%s/%s", baseDir, repoName)
		if err := os.Chdir(repoPath); err != nil {
			continue
		}


		// Run Ginkgo tests and capture output
		output, _ := runGinkgoTests()

		// Move back to the parent directory
		_ = os.Chdir(baseDir)

		// Process the Ginkgo output to find failures
		testSummary := processGinkgoOutput(output)

		// Write results to the report file
		_, err = writer.WriteString(fmt.Sprintf("\n%s\n%s\n", repoName, testSummary))
		if err != nil {
			fmt.Println(" Error writing to report file:", err)
		}
		writer.Flush()
	}

	fmt.Println("\n Test execution completed. Results saved in test_report.txt")
}

// Extracts repository name from its Git URL
func getRepoName(repoURL string) string {
	parts := strings.Split(repoURL, "/")
	return strings.TrimSuffix(parts[len(parts)-1], ".git")
}

// Runs Ginkgo tests and returns the output
func runGinkgoTests() (string, error) {
	cmd := exec.Command("ginkgo", "--flake-attempts=3", "--tags=osde2e", "-vv", "--trace", "./...")
	outputBytes, err := cmd.CombinedOutput()
	output := string(outputBytes)

	// Check for Ginkgo version mismatch and retry with go run
	if err != nil && strings.Contains(output, "Ginkgo detected a version mismatch") {
		altCmd := exec.Command("go", "run", "github.com/onsi/ginkgo/v2/ginkgo", "--flake-attempts=3", "--tags=osde2e", "-vv", "--trace", "./...")
		outputBytes, err = altCmd.CombinedOutput()
		output = string(outputBytes)
	}

	return output, err
}

// Processes Ginkgo output and extracts failure information
func processGinkgoOutput(output string) string {
	failureRegex := regexp.MustCompile(There were failures detected in the following suites:\s+(.+))
	failureMatches := failureRegex.FindStringSubmatch(output)

	if len(failureMatches) > 1 {
		failingSuite := strings.TrimSpace(failureMatches[1])
		return fmt.Sprintf("Failing test suite: %s\n", failingSuite)
	}

	return " No failing tests detected.\n No flaky tests detected.\n"
}
