package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	apigeeBackupDir      = "/tmp/apigee_backup"
	defaultRetentionDays = 7
	logFilePath          = "/var/log/apigee.log"
	maxLogFileSize       = 10 * 1024 * 1024 // 10MB
)

var webhookURL string
var tagIDs []string
var workspaceWebhookURL string

type ProjectStatus struct {
	Project string
	Status  string
	Reason  string
}

func main() {
	// Command-line flags
	projectFile := flag.String("f", "", "File containing list of Google Cloud project IDs")
	gcsBucket := flag.String("gcs", "", "GCS bucket name")
	token := flag.String("token", "", "Authorization token for Apigee")
	retentionDays := flag.Int("retention", defaultRetentionDays, "Retention period in days")
	webhook := flag.String("webhook", "", "Discord webhook URL")
	tagid := flag.String("tagid", "", "Comma-separated list of Discord tag IDs")
	workspaceWebhook := flag.String("workspace", "", "Google Workspace webhook URL")
	flag.Parse()

	// Validate flags
	if *projectFile == "" || *gcsBucket == "" || *token == "" {
		fmt.Println("Usage: ./apigee_backup -f PROJECT_FILE --gcs=GCS_BUCKET --token=AUTH_TOKEN --retention=RETENTION_DAYS [--webhook=WEBHOOK_URL] [--tagid=TAG_IDS] [--workspace=WORKSPACE_WEBHOOK_URL]")
		os.Exit(1)
	}

	// Set webhook and tag IDs
	webhookURL = *webhook
	if *tagid != "" {
		tagIDs = strings.Split(*tagid, ",")
	}

	// Set workspace webhook URL
	workspaceWebhookURL = *workspaceWebhook

	// Setup logging
	setupLogging()

	// Read project file
	projects, err := readProjectFile(*projectFile)
	if err != nil {
		log.Fatalf("Failed to read project file: %v\n", err)
	}

	var statuses []ProjectStatus
	for _, project := range projects {
		status := backupProject(project, *gcsBucket, *token, *retentionDays)
		statuses = append(statuses, status)
	}

	// Send final notifications
	sendFinalNotification(statuses)
}

func readProjectFile(filePath string) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var projects []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		project := strings.TrimSpace(scanner.Text())
		if project != "" {
			projects = append(projects, project)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return projects, nil
}

func backupProject(project, gcsBucket, token string, retentionDays int) ProjectStatus {
	status := ProjectStatus{Project: project, Status: "Complete", Reason: "no issue"}
	// Set ENV to the value of project
	ENV := project

	// Delete the backup directory if it exists
	if _, err := os.Stat(apigeeBackupDir); !os.IsNotExist(err) {
		err := os.RemoveAll(apigeeBackupDir)
		if err != nil {
			log.Printf("Failed to delete existing backup directory: %v\n", err)
			status.Status = "Failed"
			status.Reason = fmt.Sprintf("Failed to delete existing backup directory: %v", err)
			return status
		}
	}

	// Create backup directory
	err := os.MkdirAll(apigeeBackupDir, os.ModePerm)
	if err != nil {
		log.Printf("Failed to create backup directory: %v\n", err)
		status.Status = "Failed"
		status.Reason = fmt.Sprintf("Failed to create backup directory: %v", err)
		return status
	}

	// Get current date
	today := time.Now().Format("2006-01-02")

	// Check if a backup for today already exists in GCS
	if backupExistsInGCS(gcsBucket, today, ENV) {
		log.Printf("Backup for %s already exists in GCS. Skipping new backup.\n", today)
		return status
	}

	// Create date folder
	dateFolder := filepath.Join(apigeeBackupDir, today)
	err = os.MkdirAll(dateFolder, os.ModePerm)
	if err != nil {
		log.Printf("Failed to create date folder: %v\n", err)
		status.Status = "Failed"
		status.Reason = fmt.Sprintf("Failed to create date folder: %v", err)
		return status
	}

	// Backup Apigee data using apigeecli
	exportFolder := filepath.Join(apigeeBackupDir, "export")
	err = os.MkdirAll(exportFolder, os.ModePerm)
	if err != nil {
		log.Printf("Failed to create export folder: %v\n", err)
		status.Status = "Failed"
		status.Reason = fmt.Sprintf("Failed to create export folder: %v", err)
		return status
	}

	// Capture the output of the command
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.Command("bash", "-c", fmt.Sprintf("cd %s && apigeecli organizations export --all -o %s -t %s", exportFolder, project, token))
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		log.Printf("Failed to execute apigeecli command: %v\n", err)
		errorMessage := parseError(stderr.String())
		if !strings.Contains(errorMessage, "FAILED_PRECONDITION") {
			status.Status = "Failed"
			status.Reason = errorMessage
			sendDiscordNotification(project, today, "Failed", errorMessage)
			sendWorkspaceNotification(project, fmt.Sprintf("apigee-%s", project), "Failed", errorMessage)
			return status
		}
		log.Printf("Continuing despite FAILED_PRECONDITION error: %v\n", errorMessage)
	}

	// Zip the backup folder
	zipFile := filepath.Join(dateFolder, fmt.Sprintf("backup_%s_%s.zip", ENV, today))
	err = zipFolder(exportFolder, zipFile)
	if err != nil {
		log.Printf("Failed to zip folder: %v\n", err)
		status.Status = "Failed"
		status.Reason = fmt.Sprintf("Failed to zip folder: %v", err)
		return status
	}

	// Upload backup to GCS
	err = uploadToGCS(gcsBucket, zipFile, ENV)
	if err != nil {
		log.Printf("Failed to upload backup to GCS: %v\n", err)
		status.Status = "Failed"
		status.Reason = fmt.Sprintf("Failed to upload backup to GCS: %v", err)
		return status
	}

	// Send notifications for each project
	sendDiscordNotification(project, today, "Complete", "no issue")
	sendWorkspaceNotification(project, fmt.Sprintf("apigee-%s", project), "Complete", "no issue")

	// Cleanup old backups
	err = cleanupOldBackups(gcsBucket, retentionDays, ENV)
	if err != nil {
		log.Printf("Failed to clean up old backups: %v\n", err)
		status.Status = "Failed"
		status.Reason = fmt.Sprintf("Failed to clean up old backups: %v", err)
	}

	return status
}

func setupLogging() {
	logFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		fmt.Printf("Failed to open log file: %v\n", err)
		os.Exit(1)
	}
	log.SetOutput(io.MultiWriter(logFile, os.Stdout))

	// Check log file size and rotate if necessary
	stat, err := logFile.Stat()
	if err == nil && stat.Size() >= maxLogFileSize {
		rotateLogs()
	}
}

func rotateLogs() {
	for i := 9; i >= 1; i-- {
		oldLog := fmt.Sprintf("/var/log/apigee%d.zip", i)
		newLog := fmt.Sprintf("/var/log/apigee%d.zip", i+1)
		if _, err := os.Stat(oldLog); err == nil {
			os.Rename(oldLog, newLog)
		}
	}
	zipCmd := exec.Command("zip", "-r", "/var/log/apigee1.zip", logFilePath)
	zipCmd.Stdout = os.Stdout
	zipCmd.Stderr = os.Stderr
	zipCmd.Run()
	os.Remove(logFilePath)
}

func sendDiscordNotification(project, date, status, reason string) {
	if webhookURL == "" {
		return
	}

	if reason == "" {
		reason = "no issue"
	}

	content := fmt.Sprintf("**%s** (`apigee-%s`) - %s", project, project, status)
	if reason != "" {
		content = fmt.Sprintf("%s\nReason: %s", content, reason)
	}
	if len(tagIDs) > 0 {
		tags := make([]string, len(tagIDs))
		for i, id := range tagIDs {
			tags[i] = fmt.Sprintf("<@%s>", id)
		}
		tagMessage := strings.Join(tags, " ")
		content = fmt.Sprintf("%s\n\n%s", content, tagMessage)
	}

	embed := map[string]interface{}{
		"title":       fmt.Sprintf("Apigee Backup Notification %s", date),
		"description": content,
		"color":       16711680, // Red color
		"footer": map[string]interface{}{
			"text": "Note : Project - Apigee - Status",
		},
	}

	discordMessage := map[string]interface{}{
		"content": "",
		"embeds":  []map[string]interface{}{embed},
	}

	messageJSON, err := json.Marshal(discordMessage)
	if err != nil {
		log.Printf("Failed to marshal Discord message: %v\n", err)
		return
	}

	resp, err := http.Post(webhookURL, "application/json", bytes.NewBuffer(messageJSON))
	if err != nil {
		log.Printf("Failed to send Discord notification: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		log.Printf("Failed to send Discord notification, received status code: %d\n", resp.StatusCode)
	}
}

func sendWorkspaceNotification(project, dataset, status, reason string) {
	if workspaceWebhookURL == "" {
		return
	}

	if reason == "" {
		reason = "no issue"
	}

	message := fmt.Sprintf("*Apigee Daily Backup %s*\n\n*| `Project` | `Apigee-Orgs` | `Status` | `Reason` |*\n|---|---|---|\n| `%s` | `%s` | `%s` | `%s` |", time.Now().Format("2006-01-02"), project, dataset, status, reason)

	workspaceMessage := map[string]string{"text": message}
	workspaceMessageJSON, err := json.Marshal(workspaceMessage)
	if err != nil {
		log.Printf("Failed to marshal Google Workspace message: %v\n", err)
		return
	}

	resp, err := http.Post(workspaceWebhookURL, "application/json", bytes.NewBuffer(workspaceMessageJSON))
	if err != nil {
		log.Printf("Failed to send Google Workspace notification: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Failed to send Google Workspace notification, received status code: %d\n", resp.StatusCode)
	}
}

func sendFinalNotification(statuses []ProjectStatus) {
	date := time.Now().Format("2006-01-02")

	// Send final Discord notification
	if webhookURL != "" {
		content := fmt.Sprintf("**Apigee Backup Summary %s**", date)
		for _, status := range statuses {
			content = fmt.Sprintf("%s\n* **%s** - %s (`%s`)", content, status.Project, status.Status, status.Reason)
		}

		embed := map[string]interface{}{
			"title":       fmt.Sprintf("Apigee Backup Summary %s", date),
			"description": content,
			"color":       65280, // Green color
			"footer": map[string]interface{}{
				"text": "Note : Project - Status - Reason",
			},
		}

		discordMessage := map[string]interface{}{
			"content": "",
			"embeds":  []map[string]interface{}{embed},
		}

		messageJSON, err := json.Marshal(discordMessage)
		if err != nil {
			log.Printf("Failed to marshal final Discord message: %v\n", err)
			return
		}

		resp, err := http.Post(webhookURL, "application/json", bytes.NewBuffer(messageJSON))
		if err != nil {
			log.Printf("Failed to send final Discord notification: %v\n", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent {
			log.Printf("Failed to send final Discord notification, received status code: %d\n", resp.StatusCode)
		}
	}

	// Send final Workspace notification
	if workspaceWebhookURL != "" {
		content := fmt.Sprintf("*Apigee Daily Backup Summary %s*\n\n*| `Project` | `Status` | `Reason` |*\n|---|---|---|\n", date)
		for _, status := range statuses {
			content = fmt.Sprintf("%s| `%s` | `%s` | `%s` |\n", content, status.Project, status.Status, status.Reason)
		}

		workspaceMessage := map[string]string{"text": content}
		workspaceMessageJSON, err := json.Marshal(workspaceMessage)
		if err != nil {
			log.Printf("Failed to marshal final Google Workspace message: %v\n", err)
			return
		}

		resp, err := http.Post(workspaceWebhookURL, "application/json", bytes.NewBuffer(workspaceMessageJSON))
		if err != nil {
			log.Printf("Failed to send final Google Workspace notification: %v\n", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("Failed to send final Google Workspace notification, received status code: %d\n", resp.StatusCode)
		}
	}
}

func backupExistsInGCS(gcsBucket, date, env string) bool {
	// Check for the backup existence in the GCS bucket
	cmd := exec.Command("gsutil", "ls", fmt.Sprintf("gs://%s/%s/%s/", gcsBucket, env, date))
	err := cmd.Run()
	return err == nil
}

func uploadToGCS(gcsBucket, sourceFile, env string) error {
	// Upload the backup to GCS
	destDir := fmt.Sprintf("gs://%s/%s/%s", gcsBucket, env, filepath.Base(sourceFile))
	cmd := exec.Command("gsutil", "cp", sourceFile, destDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func cleanupOldBackups(gcsBucket string, retentionDays int, env string) error {
	// List objects in GCS bucket
	cmd := exec.Command("gsutil", "ls", fmt.Sprintf("gs://%s/%s/", gcsBucket, env))
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to list GCS bucket: %w", err)
	}

	// Calculate cutoff date
	cutoffDate := time.Now().AddDate(0, 0, -retentionDays)

	// Parse and delete old backups
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		if isOlderThanRetention(line, cutoffDate, env) {
			cmd := exec.Command("gsutil", "rm", line)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err := cmd.Run()
			if err != nil {
				log.Printf("Failed to delete old backup %s: %v\n", line, err)
			} else {
				log.Printf("Deleted old backup %s\n", line)
			}
		}
	}
	return nil
}

func isOlderThanRetention(gcsPath string, cutoffDate time.Time, env string) bool {
	// Extract date from GCS path
	// Assuming path format: gs://bucket/env/backup_YYYY-MM-DD.zip
	base := filepath.Base(gcsPath)
	dateStr := base[len(fmt.Sprintf("backup_%s_", env)) : len(base)-len(".zip")]

	backupDate, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		log.Printf("Failed to parse date from path %s: %v\n", gcsPath, err)
		return false
	}

	return backupDate.Before(cutoffDate)
}

func zipFolder(sourceDir, zipFile string) error {
	zipCmd := exec.Command("zip", "-r", zipFile, ".", "-i", "*")
	zipCmd.Dir = sourceDir
	zipCmd.Stdout = os.Stdout
	zipCmd.Stderr = os.Stderr
	return zipCmd.Run()
}

func parseError(stderr string) string {
	// Parsing the error message to extract meaningful information
	var parsedError map[string]interface{}
	if err := json.Unmarshal([]byte(stderr), &parsedError); err == nil {
		if errorInfo, ok := parsedError["error"].(map[string]interface{}); ok {
			if status, exists := errorInfo["status"].(string); exists && status == "FAILED_PRECONDITION" {
				return "FAILED_PRECONDITION - Continuing without interrupting the process"
			}
			if message, exists := errorInfo["message"].(string); exists {
				return message
			}
		}
	}
	if strings.Contains(stderr, "Unauthorized - the client must authenticate itself") {
		return "Unauthorized - the client must authenticate itself"
	}
	return stderr
}
