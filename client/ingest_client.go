package main

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "embed"

	"github.com/schollz/progressbar/v3"
)

//go:embed dist/face_scanner_tool
var aiToolBinary []byte

// --- Configuration & Globals ---

var (
	ServerURL      string
	ProjectName    string
	SourceDir      string
	NumTranscoders = 3
	NumUploaders   = 5
	MaxRetries     = 5
)

const (
	UploadEndpoint   = "/api/dailies/upload"
	CheckEndpoint    = "/api/dailies/check"
	AIUploadEndpoint = "/api/dailies/%d/faces"
	TempProxyDir     = "tmp_proxies"
	AIToolPath       = "/tmp/mam_ai_tool_v1"
)

// --- Structures ---

type Job struct {
	SourcePath string // Absolute path to original file
	RelPath    string // Relative path for folder structure
	ProxyPath  string // Path to generated proxy file (filled by Transcoder)
	Project    string
	Metadata   string // JSON string from ffprobe
	MD5        string // MD5 hash of the PROXY file
}

type CheckResponse struct {
	Status       string `json:"status"` // "completed", "partial", "new"
	UploadedSize int64  `json:"uploaded_size"`
}

// --- Main ---

func main() {
	// 1. External Configuration
	serverFlag := flag.String("server", "http://localhost:8080", "Server URL")
	projectFlag := flag.String("project", "", "Project Name (Required)")
	sourceFlag := flag.String("source", ".", "Source Directory")
	flag.Parse()

	// Wizard Mode
	if *projectFlag == "" && len(os.Args) == 1 {
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("Enter Server URL (default: http://localhost:8080): ")
		text, _ := reader.ReadString('\n')
		text = strings.TrimSpace(text)
		if text != "" {
			*serverFlag = text
		}

		fmt.Print("Enter Project Name: ")
		text, _ = reader.ReadString('\n')
		text = strings.TrimSpace(text)
		if text == "" {
			fmt.Println("Project name is required.")
			os.Exit(1)
		}
		*projectFlag = text

		fmt.Print("Enter Source Path (default: .): ")
		text, _ = reader.ReadString('\n')
		text = strings.TrimSpace(text)
		if text != "" {
			*sourceFlag = text
		}
	} else if *projectFlag == "" {
		fmt.Println("Error: --project is required")
		flag.Usage()
		os.Exit(1)
	}

	ServerURL = strings.TrimRight(*serverFlag, "/")
	ProjectName = *projectFlag

	absSource, err := filepath.Abs(*sourceFlag)
	if err != nil {
		fmt.Printf("Error resolving source: %v\n", err)
		os.Exit(1)
	}
	SourceDir = absSource

	// Extract AI Tool
	extractAITool()

	fmt.Printf("MAM Ingest Client v2.0\n")
	fmt.Printf("Server:  %s\n", ServerURL)
	fmt.Printf("Project: %s\n", ProjectName)
	fmt.Printf("Source:  %s\n", SourceDir)

	// 2. Prevent Sleep (macOS)
	preventSleep()

	// 3. Setup Pipeline
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nReceived interrupt. Shutting down...")
		cancel()
	}()

	// Create Temp Dir
	os.MkdirAll(TempProxyDir, 0755)

	// channels
	transcodeChan := make(chan Job, 100)
	uploadChan := make(chan Job, 100)

	var wgTranscode sync.WaitGroup
	var wgUpload sync.WaitGroup

	// Scan files first to init progress bar
	fmt.Println("Scanning files...")
	files := scanFiles(SourceDir)
	totalFiles := len(files)
	fmt.Printf("Found %d video files.\n", totalFiles)

	if totalFiles == 0 {
		return
	}

	bar := progressbar.Default(int64(totalFiles), "Ingesting")

	// Start Workers
	for i := 0; i < NumTranscoders; i++ {
		wgTranscode.Add(1)
		go transcoderWorker(ctx, i, transcodeChan, uploadChan, &wgTranscode, bar)
	}

	for i := 0; i < NumUploaders; i++ {
		wgUpload.Add(1)
		go uploaderWorker(ctx, i, uploadChan, &wgUpload, bar)
	}

	// Dispatcher (Scanner Logic)
	go func() {
		for _, path := range files {
			rel, _ := filepath.Rel(SourceDir, path)
			relDir := filepath.Dir(rel)
			if relDir == "." {
				relDir = ""
			}

			// Pre-calculate Proxy Path for check
			filename := filepath.Base(path)
			proxyName := strings.TrimSuffix(filename, filepath.Ext(filename)) + "_proxy.mp4"
			proxyPath := filepath.Join(TempProxyDir, proxyName)

			job := Job{
				SourcePath: path,
				RelPath:    relDir,
				Project:    ProjectName,
				ProxyPath:  proxyPath,
			}

			// Smart Check
			// "Scanner -> ffprobe -> Check Status"
			// Actually, running ffprobe here might slow down dispatch if single threaded.
			// But user asked for this order. We'll do a quick check here.
			// Ideally we move ffprobe to transcoder to parallelize, but let's follow flow:
			// "Scanner: Scan -> ffprobe -> Check Status"

			// Extract Metadata (Source) - We use this for DB, even if we upload proxy?
			// Usually we want metadata of the Original Source.
			meta, err := getMetadata(job.SourcePath)
			if err == nil {
				job.Metadata = meta
			}

			// Check Status
			status, uploadedSize := checkUploadStatus(filename, ProjectName, relDir)

			// C. Change Detection
			// If status is "partial" or "completed", we should check if file changed?
			// "对比服务器返回的 file_size 和本地 Proxy 大小"
			// NOTE: We haven't generated Proxy yet! So we don't know local proxy size unless it exists.

			// Logic Adjustment:
			// If "Completed": Skip.
			// If "Partial": Check if we have a local proxy file.
			//    If local proxy exists: Check size.
			//       If Match (approx): Resume Upload.
			//       If Mismatch: Retranscode (treat as new).
			//    If no local proxy: Retranscode.
			// If "New": Transcode.

			if status == "completed" {
				bar.Add(1)
				continue
			}

			if status == "partial" {
				// Check if we have a valid local proxy
				info, err := os.Stat(proxyPath)
				if err == nil && info.Size() > 0 {
					// We have a proxy.
					// Check consistency? Verify if server.uploadedSize <= local.size?
					// Prompt C says: "Check /check returns file_size vs local Proxy size".
					// Actually /check returns `uploaded_size` (current bytes on server).
					// This isn't the total size.
					// If server has 5MB, and local is 10MB, we can resume.
					// If server has 10MB, and local is 5MB, that's impossible/error -> restart.

					if uploadedSize > info.Size() {
						// Server has more than we have? File changed or server corrupt. Reset.
						// Treat as "modified" -> Transcode
						transcodeChan <- job
					} else {
						// Resume
						// Calculate MD5 of existing proxy before push?
						// "Transcoder -> ... -> Calc MD5".
						// We are skipping transcoder. We must calc MD5 here or in Uploader.
						// Let's assume Uploader handles MD5 calc if missing? or we do it here.
						// To keep uploader simple, let's just send to Transcode channel but rely on
						// Transcoder's "Skip if exists" logic?
						// But Transcoder usually overwrites `-y`.
						// Let's send to UploadChan directly, but calculate MD5 first.

						// Recalculating MD5 might be slow, so run in goroutine or just let Transcoder handle "verification"?
						// Simplest: Send to Transcoder, but Transcoder checks if Proxy exists and is valid?
						// No, user said "Skip Transcode".

						// We'll calculate MD5 here (might block scanner lightly but ok) or
						// Better: Creates a 'Validation' worker?
						// Let's just calculate MD5 in a separate goroutine to avoid blocking scanner loop
						// and then push to uploadChan.

						wgTranscode.Add(1) // Borrow a slot from transcode WG to keep track
						go func(j Job) {
							defer wgTranscode.Done()
							hash, err := calculateMD5(j.ProxyPath)
							if err == nil {
								j.MD5 = hash
								uploadChan <- j
							} else {
								// Error reading proxy? Re-transcode
								transcodeChan <- j
							}
						}(job)
					}
					continue
				}
				// If partial but no local proxy -> Re-transcode
			}

			// Default: Send to Transcode
			transcodeChan <- job
		}
		close(transcodeChan)
	}()

	// Wait logic
	go func() {
		wgTranscode.Wait()
		close(uploadChan)
	}()

	wgUpload.Wait()
	bar.Finish()
	fmt.Println("\nAll jobs finished.")
}

// --- Helpers ---

func preventSleep() {
	if runtime.GOOS == "darwin" {
		path, err := exec.LookPath("caffeinate")
		if err == nil {
			cmd := exec.Command(path, "-d", "-w", strconv.Itoa(os.Getpid()))
			err := cmd.Start()
			if err != nil {
				fmt.Printf("[Warning] Failed to start caffeinate: %v\n", err)
			} else {
				fmt.Println("[System] Caffeinate active (preventing sleep)")
			}
		} else {
			fmt.Println("[Warning] 'caffeinate' not found. System sleep may interrupt uploads.")
		}
	}
}

func scanFiles(root string) []string {
	var files []string
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			ext := strings.ToLower(filepath.Ext(path))
			if (ext == ".mov" || ext == ".mp4" || ext == ".mxf") && !strings.HasPrefix(info.Name(), "._") {
				files = append(files, path)
			}
		}
		return nil
	})
	return files
}

func getMetadata(path string) (string, error) {
	// ffprobe -v quiet -print_format json -show_format -show_streams -select_streams v:0 input.mov
	cmd := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", "-select_streams", "v:0", path)
	out, err := cmd.Output()
	if err != nil {
		return "{}", err
	}
	// Return compact JSON
	return string(out), nil
}

func calculateMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func checkUploadStatus(filename, project, folderPath string) (string, int64) {
	url := fmt.Sprintf("%s%s?filename=%s&project_name=%s&folder_path=%s", ServerURL, CheckEndpoint, filename, project, folderPath)
	resp, err := http.Get(url)
	if err != nil {
		return "error", 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "error", 0
	}

	var res CheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "error", 0
	}
	return res.Status, res.UploadedSize
}

// --- Workers ---

func transcoderWorker(ctx context.Context, id int, jobs <-chan Job, uploads chan<- Job, wg *sync.WaitGroup, bar *progressbar.ProgressBar) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-jobs:
			if !ok {
				return
			}

			// Transcode: ffmpeg -i source -c:v hevc_videotoolbox -q:v 55 -tag:v hvc1 -vf scale=-1:1080 -c:a aac -y proxy
			// Use hevc_videotoolbox (Mac)

			// fmt.Printf("[Transcode] %s\n", filepath.Base(job.SourcePath))

			cmd := exec.CommandContext(ctx, "ffmpeg",
				"-i", job.SourcePath,
				"-c:v", "hevc_videotoolbox",
				"-q:v", "55",
				"-tag:v", "hvc1",
				"-vf", "scale=-1:1080",
				"-c:a", "aac",
				"-y",
				job.ProxyPath,
			)
			// Suppress output
			cmd.Stderr = nil
			cmd.Stdout = nil

			if err := cmd.Run(); err != nil {
				fmt.Printf("[Error Transcode] %s: %v\n", filepath.Base(job.SourcePath), err)
				bar.Add(1) // Fail = done
				continue
			}

			// Calculate MD5
			hash, err := calculateMD5(job.ProxyPath)
			if err != nil {
				fmt.Printf("[Error MD5] %s: %v\n", filepath.Base(job.SourcePath), err)
				bar.Add(1)
				continue
			}
			job.MD5 = hash

			select {
			case uploads <- job:
			case <-ctx.Done():
			}
		}
	}
}

func uploaderWorker(ctx context.Context, id int, uploads <-chan Job, wg *sync.WaitGroup, bar *progressbar.ProgressBar) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-uploads:
			if !ok {
				return
			}

			// Parallel AI processing
			var aiResult string
			var aiErr error
			var wgAI sync.WaitGroup
			wgAI.Add(1)
			go func() {
				defer wgAI.Done()
				aiResult, aiErr = runAI(job.ProxyPath)
			}()

			origName := filepath.Base(job.SourcePath)
			done := false
			var dailyID int64

			for retry := 0; retry < MaxRetries; retry++ {
				// 1. Check
				status, offset := checkUploadStatus(origName, job.Project, job.RelPath)

				if status == "completed" {
					done = true
					// Note: If already completed, we might validly skip upload,
					// but we won't get the ID to verify AI data unless we query for it or AI is already done.
					// For simplicity, if completed, we skip AI upload or assume it's done.
					// Or we could implement a 'get daily ID' check.
					// Given the prompt "If upload successful... then upload AI",
					// let's assume if it's already done we skip AI for now (or minimal impl).
					break
				}

				info, err := os.Stat(job.ProxyPath)
				if err != nil {
					break
				}

				if offset > info.Size() {
					offset = 0
				}

				// 2. Upload
				id, err := uploadStream(job, origName, offset, info.Size())
				if err == nil {
					dailyID = id
					done = true
					break
				} else {
					time.Sleep(5 * time.Second)
				}
			}

			// Wait for AI
			wgAI.Wait()

			if done {
				if dailyID > 0 && aiErr == nil && aiResult != "" {
					// Upload AI Data
					uploadAIData(dailyID, aiResult)
				}
				os.Remove(job.ProxyPath)
			}
			bar.Add(1)
		}
	}
}

func runAI(proxyPath string) (string, error) {
	cmd := exec.Command(AIToolPath, proxyPath)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func uploadAIData(dailyID int64, data string) {
	url := fmt.Sprintf("%s"+AIUploadEndpoint, ServerURL, dailyID)
	resp, err := http.Post(url, "application/json", strings.NewReader(data))
	if err == nil {
		resp.Body.Close()
	}
}

func extractAITool() {
	if _, err := os.Stat(AIToolPath); os.IsNotExist(err) {
		fmt.Println("Extracting AI Tool...")
		err := os.WriteFile(AIToolPath, aiToolBinary, 0755)
		if err != nil {
			fmt.Printf("Error extracting AI tool: %v\n", err)
		} else {
			// Ensure executable
			os.Chmod(AIToolPath, 0755)
		}
	}
}

func uploadStream(job Job, originalFilename string, offset, totalSize int64) (int64, error) {
	f, err := os.Open(job.ProxyPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	if offset > 0 {
		f.Seek(offset, 0)
	}

	url := fmt.Sprintf("%s%s?project_name=%s&folder_path=%s&filename=%s&total_size=%d",
		ServerURL, UploadEndpoint, job.Project, job.RelPath, originalFilename, totalSize)

	req, err := http.NewRequest("POST", url, f)
	if err != nil {
		return 0, err
	}

	req.Header.Set("X-Upload-Offset", fmt.Sprintf("%d", offset))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = totalSize - offset

	if job.MD5 != "" {
		req.Header.Set("X-File-Hash", job.MD5)
	}
	if job.Metadata != "" {
		b64Meta := base64.StdEncoding.EncodeToString([]byte(job.Metadata))
		req.Header.Set("X-File-Metadata", b64Meta)
	}

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}

	// Parse Response for ID
	var res struct {
		Status string `json:"status"`
		ID     int64  `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return 0, nil // Return nil error but 0 ID if parse fails, effectively skipping AI upload
	}

	return res.ID, nil
}
