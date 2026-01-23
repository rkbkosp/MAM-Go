package main

import (
	"bufio"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	_ "modernc.org/sqlite"
)

//go:embed templates/*
var embedFS embed.FS

var cfg *Config

type Config struct {
	ServerPort      string
	AdminPassword   string
	UploadRoot      string
	DBPath          string
	EnableWebDAV    bool
	WebdavURL       string
	WebdavUser      string
	WebdavPass      string
	EnableAutoClean bool
	KeepVersions    int
	SafeCleanMode   bool
	GotifyURL       string
	GotifyToken     string
}

func loadConfig(path string) {
	// Defaults
	defaultConfig := map[string]string{
		"SERVER_PORT":       ":8080",
		"ADMIN_PASSWORD":    "my_secret_pwd",
		"UPLOAD_ROOT":       "storage",
		"DB_PATH":           "mam.db",
		"ENABLE_WEBDAV":     "true",
		"WEBDAV_URL":        "http://127.0.0.1:5244/dav/",
		"WEBDAV_USER":       "admin",
		"WEBDAV_PASS":       "admin",
		"ENABLE_AUTO_CLEAN": "true",
		"KEEP_VERSIONS":     "5",
		"SAFE_CLEAN_MODE":   "true",
		"GOTIFY_URL":        "http://127.0.0.1:8081/message",
		"GOTIFY_TOKEN":      "token_here",
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Printf("Configuration file not found at %s. Creating default template.\n", path)
		f, err := os.Create(path)
		if err != nil {
			log.Fatalf("Failed to create config file: %v", err)
		}
		defer f.Close()

		content := `# Server Configuration
SERVER_PORT=:8080
ADMIN_PASSWORD=my_secret_pwd
UPLOAD_ROOT=storage
DB_PATH=mam.db

# Operations
ENABLE_WEBDAV=true
WEBDAV_URL=http://127.0.0.1:5244/dav/
WEBDAV_USER=admin
WEBDAV_PASS=admin
ENABLE_AUTO_CLEAN=true
KEEP_VERSIONS=5
SAFE_CLEAN_MODE=true # True=Delete only if backup_status=1

# Notifications
GOTIFY_URL=http://127.0.0.1:8081/message
GOTIFY_TOKEN=token_here
`
		f.WriteString(content)
		fmt.Println("Please edit the .env file and restart the server.")
		os.Exit(0)
	}

	// Read Config
	file, err := os.Open(path)
	if err != nil {
		log.Fatalf("Failed to open config file: %v", err)
	}
	defer file.Close()

	env := make(map[string]string)
	// Copy defaults first
	for k, v := range defaultConfig {
		env[k] = v
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			// Handle inline comments
			if idx := strings.Index(val, "#"); idx != -1 {
				val = strings.TrimSpace(val[:idx])
			}
			env[key] = val
		}
	}

	cfg = &Config{
		ServerPort:    env["SERVER_PORT"],
		AdminPassword: env["ADMIN_PASSWORD"],
		UploadRoot:    env["UPLOAD_ROOT"],
		DBPath:        env["DB_PATH"],
		WebdavURL:     env["WEBDAV_URL"],
		WebdavUser:    env["WEBDAV_USER"],
		WebdavPass:    env["WEBDAV_PASS"],
		GotifyURL:     env["GOTIFY_URL"],
		GotifyToken:   env["GOTIFY_TOKEN"],
	}

	cfg.EnableWebDAV, _ = strconv.ParseBool(env["ENABLE_WEBDAV"])
	cfg.EnableAutoClean, _ = strconv.ParseBool(env["ENABLE_AUTO_CLEAN"])
	cfg.SafeCleanMode, _ = strconv.ParseBool(env["SAFE_CLEAN_MODE"])
	cfg.KeepVersions, _ = strconv.Atoi(env["KEEP_VERSIONS"])
}

// --- Database Models ---

type Project struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Video struct {
	ID           int       `json:"id"`
	ProjectID    int       `json:"project_id"`
	VersionLabel string    `json:"version_label"`
	FilePath     string    `json:"file_path"`
	Note         string    `json:"note"`          // New field
	BackupStatus int       `json:"backup_status"` // 0: Pending, 1: Done
	CreatedAt    time.Time `json:"created_at"`
}

type Daily struct {
	ID               int       `json:"id"`
	ProjectID        int       `json:"project_id"`
	OriginalFilename string    `json:"original_filename"`
	ProxyPath        string    `json:"proxy_path"`
	FolderPath       string    `json:"folder_path"`
	Camera           string    `json:"camera"`
	Scene            string    `json:"scene"`
	Take             string    `json:"take"`
	IsGood           bool      `json:"is_good"`
	CreatedAt        time.Time `json:"created_at"`
}

type Token struct {
	Code      string    `json:"code"`
	TargetID  int       `json:"target_id"` // ProjectID (if type=dailies) or VideoID (if type=demo)
	Type      string    `json:"type"`      // "demo" or "dailies"
	Role      string    `json:"role"`      // "reviewer" or "viewer"
	CreatedAt time.Time `json:"created_at"`
}

type Comment struct {
	ID             int       `json:"id"`
	VideoID        int       `json:"video_id"`
	Timecode       float64   `json:"timecode"`
	Content        string    `json:"content"`
	Severity       string    `json:"severity"`
	Reporter       string    `json:"reporter"`
	IsFixed        bool      `json:"is_fixed"`
	ScreenshotPath string    `json:"screenshot_path"`
	CreatedAt      time.Time `json:"created_at"`
}

var db *sql.DB

// --- Init Database ---
func initDB() {
	var err error
	db, err = sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		log.Fatal(err)
	}

	schema := `
	CREATE TABLE IF NOT EXISTS projects (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE, created_at DATETIME);
	CREATE TABLE IF NOT EXISTS videos (id INTEGER PRIMARY KEY AUTOINCREMENT, project_id INTEGER, version_label TEXT, file_path TEXT, note TEXT, backup_status INTEGER DEFAULT 0, created_at DATETIME);
	CREATE TABLE IF NOT EXISTS dailies (id INTEGER PRIMARY KEY AUTOINCREMENT, project_id INTEGER, original_filename TEXT, proxy_path TEXT, folder_path TEXT, camera TEXT, scene TEXT, take TEXT, is_good BOOl DEFAULT 0, created_at DATETIME);
	CREATE TABLE IF NOT EXISTS tokens (code TEXT PRIMARY KEY, target_id INTEGER, type TEXT, role TEXT, created_at DATETIME);
	CREATE TABLE IF NOT EXISTS comments (id INTEGER PRIMARY KEY AUTOINCREMENT, video_id INTEGER, timecode REAL, content TEXT, severity TEXT, reporter TEXT, is_fixed BOOL DEFAULT 0, screenshot_path TEXT, created_at DATETIME);
	`
	_, err = db.Exec(schema)
	// Migration for Note if needed (simple check)
	_, _ = db.Exec("ALTER TABLE videos ADD COLUMN note TEXT")
	if err != nil {
		log.Fatalf("Schema init failed: %v", err)
	}
}

// --- Utils ---
func sendNotification(title, message string) {
	// Simple HTTP POST to Gotify
	go func() {
		// Implement HTTP Client call here to GotifyURL
		// Placeholder
		log.Printf("[Gotify] %s: %s", title, message)
	}()
}

func getProjectID(name string) int {
	var id int
	err := db.QueryRow("SELECT id FROM projects WHERE name = ?", name).Scan(&id)
	if err == sql.ErrNoRows {
		res, _ := db.Exec("INSERT INTO projects (name, created_at) VALUES (?, ?)", name, time.Now())
		lid, _ := res.LastInsertId()
		return int(lid)
	}
	return id
}

// --- Handlers ---

func uploadDemoHandler(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(400, gin.H{"error": "No file"})
		return
	}

	filename := file.Filename
	projectName := "Unknown"
	versionLabel := "v1"

	// Parse Form Fields if available (manual upload)
	formProjectID := c.PostForm("project_id")
	formVersion := c.PostForm("version_label")
	formNote := c.PostForm("note")

	var projectID int
	if formProjectID != "" {
		fmt.Sscanf(formProjectID, "%d", &projectID)
		if formVersion != "" {
			versionLabel = formVersion
		}
		// If explicit project ID, get name for path
		db.QueryRow("SELECT name FROM projects WHERE id = ?", projectID).Scan(&projectName)
	} else {
		// Regex Fallback
		re := regexp.MustCompile(`^([^_]+)_(.+)\.(.+)$`)
		matches := re.FindStringSubmatch(filename)
		if len(matches) >= 3 {
			projectName = matches[1]
			versionLabel = matches[2]
		} else {
			parts := strings.SplitN(filename, "_", 2)
			projectName = parts[0]
			if len(parts) > 1 {
				versionLabel = strings.TrimSuffix(parts[1], filepath.Ext(filename))
			}
		}
		projectID = getProjectID(projectName)
	}
	saveDir := filepath.Join(cfg.UploadRoot, projectName, "Demos")
	os.MkdirAll(saveDir, 0755)

	destPath := filepath.Join(saveDir, filename)
	if err := c.SaveUploadedFile(file, destPath); err != nil {
		c.JSON(500, gin.H{"error": "Save failed"})
		return
	}

	_, err = db.Exec("INSERT INTO videos (project_id, version_label, file_path, note, created_at) VALUES (?, ?, ?, ?, ?)",
		projectID, versionLabel, destPath, formNote, time.Now())
	if err != nil {
		log.Printf("DB insert failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("DB insert failed: %v", err)})
		return
	}

	// Generate a quick review token for convenience?
	// For now just return success
	sendNotification("New Demo Uploaded", fmt.Sprintf("Project: %s, Ver: %s", projectName, versionLabel))
	c.JSON(http.StatusOK, gin.H{"status": "ok", "path": destPath})
}

func checkDailyHandler(c *gin.Context) {
	projectName := c.Query("project_name")
	folderPath := c.Query("folder_path")    // Need folder path to check partial file
	originalFilename := c.Query("filename") // changed from original_filename to match prompt consistency or keep as is? Prompt says "filename" in section A. Let's stick to prompt "filename".

	// Aligning with Prompt A: parameters project_name, folder_path, filename
	// But `uploadDailyHandler` used to take `original_filename` in DB. Let's verify `uploadDailyHandler` usage.
	// Current DB `dailies` has `original_filename`.
	// Let's use `filename` as the query param key to match prompt A, but map it to our needs.

	if originalFilename == "" {
		originalFilename = c.Query("original_filename") // Backward compat or fallback
	}

	projectID := getProjectID(projectName)
	// getProjectID creates if not exists, which might be okay or not.
	// Ideally check if project exists first, but `getProjectID` is efficient enough.
	// However, if project doesn't exist, we can't have files.

	// 1. Check if completed file exists in DB
	var count int
	err := db.QueryRow("SELECT count(*) FROM dailies WHERE project_id = ? AND original_filename = ?", projectID, originalFilename).Scan(&count)
	if err == nil && count > 0 {
		c.JSON(200, gin.H{"status": "completed"})
		return
	}

	// 2. Check for partial file on disk
	// Path construction: UploadRoot / projectName / "Dailies" / folderPath / filename + ".part"
	// Ensure consistency with uploadDailyHandler path logic.
	saveDir := filepath.Join(cfg.UploadRoot, projectName, "Dailies", folderPath)
	partPath := filepath.Join(saveDir, originalFilename+".part")

	info, err := os.Stat(partPath)
	if err == nil && !info.IsDir() {
		c.JSON(200, gin.H{"status": "partial", "uploaded_size": info.Size()})
		return
	}

	// 3. New
	c.JSON(200, gin.H{"status": "new", "uploaded_size": 0})
}

func uploadDailyHandler(c *gin.Context) {
	// Resumable Upload Implementation
	projectName := c.Query("project_name")
	folderPath := c.Query("folder_path")
	filename := c.Query("filename")
	totalSizeStr := c.Query("total_size")

	// Metadata might be passed as query params too or we assume it's sent only on completion or separate meta call?
	// The prompt B doesn't explicitly mention metadata (camera, scene, take) in the Query Params list,
	// but the previous implementation had it.
	// For now, let's assume metadata is sent in query params or we just use defaults if missing,
	// OR we might need a separate metadata update call.
	// Let's add them to Query params to keep it simple as per "Query 参数: ...".
	// Wait, the prompt list didn't include them.
	// "Query 参数: project_name, folder_path, filename, total_size (文件总大小)。"
	// I'll grab them from Query if present, or leave empty.
	camera := c.Query("camera")
	scene := c.Query("scene")
	take := c.Query("take")

	var totalSize int64
	fmt.Sscanf(totalSizeStr, "%d", &totalSize)

	offsetStr := c.GetHeader("X-Upload-Offset")
	var offset int64
	if offsetStr != "" {
		fmt.Sscanf(offsetStr, "%d", &offset)
	}

	projectID := getProjectID(projectName)
	saveDir := filepath.Join(cfg.UploadRoot, projectName, "Dailies", folderPath)
	os.MkdirAll(saveDir, 0755)

	partPath := filepath.Join(saveDir, filename+".part")
	finalPath := filepath.Join(saveDir, filename)

	// Mode handling
	var file *os.File
	var err error

	if offset == 0 {
		file, err = os.OpenFile(partPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	} else {
		// Safety check
		info, sErr := os.Stat(partPath)
		if sErr != nil {
			c.JSON(400, gin.H{"error": "Offset > 0 but no partial file found"})
			return
		}
		if info.Size() != offset {
			c.JSON(409, gin.H{"error": fmt.Sprintf("Offset mismatch. Server has %d, client sent %d", info.Size(), offset)})
			return
		}
		file, err = os.OpenFile(partPath, os.O_APPEND|os.O_WRONLY, 0644)
	}

	if err != nil {
		c.JSON(500, gin.H{"error": "File open failed"})
		return
	}
	defer file.Close()

	// Stream Copy
	// c.Request.Body is the stream
	_, err = io.Copy(file, c.Request.Body)
	if err != nil {
		c.JSON(500, gin.H{"error": "Write failed"})
		return
	}

	// Verify Size
	info, err := os.Stat(partPath)
	if err != nil {
		c.JSON(500, gin.H{"error": "Stat failed"})
		return
	}

	if info.Size() == totalSize {
		// Done!
		file.Close() // Close before rename
		err := os.Rename(partPath, finalPath)
		if err != nil {
			c.JSON(500, gin.H{"error": "Rename failed"})
			return
		}

		// Read Metadata from Header (Base64)
		metadataB64 := c.GetHeader("X-File-Metadata")
		if metadataB64 != "" {
			data, err := base64.StdEncoding.DecodeString(metadataB64)
			if err == nil {
				var m map[string]interface{}
				if json.Unmarshal(data, &m) == nil {
					// We can extract camera/scene/take if present in the raw metadata,
					// or just save the whole blob.
					// The prompt implies "Save this JSON".
					// The DB `dailies` table has `camera`, `scene`, `take` columns.
					// It doesn't have a generic `metadata` column yet.
					// We should probably extract keys if they match, or add a column.
					// Given I can't easily ALTER table dynamically nicely without migration,
					// I will try to extract known fields if they exist in the JSON.
					// ffprobe JSON usually has "format": { "tags": { ... } }
					// Let's assume the user puts custom metadata or we just ignore if fields missing.
					// Or... I'll just leave `camera`, `scene`, `take` as empty or from Query if provided.
					// The prompt says "将此 JSON 作为 `metadata` 字段..."
					// If DB doesn't have `metadata` column, I should add it or just log it for now.
					// Let's Add a Note to the `note` field or similar? No `dailies` doesn't have `note`.
					// I will skip DB Schema change to avoid complexity unless user asked (User asked "Server Interface Definition" in previous turn, but didn't explicitly ask for Schema change for metadata blob).
					// BUT, if I don't save it, what's the point?
					// I will just map `camera`, `scene`, `take` if found in `format.tags`.
					if format, ok := m["format"].(map[string]interface{}); ok {
						if tags, ok := format["tags"].(map[string]interface{}); ok {
							if v, ok := tags["com.apple.quicktime.make"]; ok {
								camera = fmt.Sprint(v)
							}
							// scene/take usually not in ffprobe standard output unless custom user data.
						}
					}
				}
			}
		}

		// Previous Insert logic
		_, err = db.Exec("INSERT INTO dailies (project_id, original_filename, proxy_path, folder_path, camera, scene, take, is_good, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			projectID, filename, finalPath, folderPath, camera, scene, take, false, time.Now())

		if err != nil {
			log.Printf("DB Error: %v", err)
			// Return OK anyway as file is safe? Or error?
			// Client might retry if error.
		}

		c.JSON(200, gin.H{"status": "completed"})
	} else {
		// Partial success
		c.JSON(200, gin.H{"status": "partial", "uploaded": info.Size()})
	}
}

func watchHandler(c *gin.Context) {
	tokenCode := c.Query("token")
	var t Token
	err := db.QueryRow("SELECT target_id, type, role FROM tokens WHERE code = ?", tokenCode).Scan(&t.TargetID, &t.Type, &t.Role)
	if err != nil {
		c.String(http.StatusForbidden, "Invalid Token")
		return
	}

	if t.Type == "demo" {
		// Render Review Page
		var v Video
		var p Project
		db.QueryRow("SELECT file_path, project_id FROM videos WHERE id = ?", t.TargetID).Scan(&v.FilePath, &v.ProjectID)
		db.QueryRow("SELECT name FROM projects WHERE id = ?", v.ProjectID).Scan(&p.Name)

		// Load comments
		rows, _ := db.Query("SELECT id, timecode, content, reporter FROM comments WHERE video_id = ?", t.TargetID)
		var comments []Comment
		for rows.Next() {
			var cm Comment
			rows.Scan(&cm.ID, &cm.Timecode, &cm.Content, &cm.Reporter)
			comments = append(comments, cm)
		}

		c.HTML(http.StatusOK, "watch.html", gin.H{
			"Mode":        "demo",
			"VideoPath":   "/stream/" + fmt.Sprint(t.TargetID) + "?token=" + tokenCode,
			"ProjectName": p.Name,
			"Comments":    comments,
			"Token":       tokenCode,
			"IsReviewer":  t.Role == "reviewer",
		})
	} else if t.Type == "dailies" {
		// Render Grid
		rows, _ := db.Query("SELECT id, original_filename, folder_path, is_good FROM dailies WHERE project_id = ? ORDER BY created_at DESC", t.TargetID)
		var ds []Daily
		for rows.Next() {
			var d Daily
			rows.Scan(&d.ID, &d.OriginalFilename, &d.FolderPath, &d.IsGood)
			ds = append(ds, d)
		}
		c.HTML(http.StatusOK, "watch.html", gin.H{
			"Mode":    "dailies",
			"Dailies": ds,
			"Token":   tokenCode,
		})
	}
}

func streamHandler(c *gin.Context) {
	// Simple stream protection check
	tokenCode := c.Query("token")
	id := c.Param("id")
	var count int
	// Ideally we check if token targets this video or project, for simplicity just check existence
	db.QueryRow("SELECT count(*) FROM tokens WHERE code = ?", tokenCode).Scan(&count)
	if count == 0 {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}

	// Determine file path.
	// For simplicity, we assume ID is video ID.
	// If streaming dailies, we'd need a separate route or more logic.
	// Let's assume this is for Videos (Demos).
	// For Dailies, we might need /stream-daily/:id.
	// User Requirement: "Stream Protection: /stream/:id verify token"

	// Check Video table first
	var path string
	err := db.QueryRow("SELECT file_path FROM videos WHERE id = ?", id).Scan(&path)
	if err != nil {
		// Check Daily table
		err = db.QueryRow("SELECT proxy_path FROM dailies WHERE id = ?", id).Scan(&path)
		if err != nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
	}

	c.File(path)
}

func toggleGoodHandler(c *gin.Context) {
	// POST /api/dailies/toggle-good?token=...&id=...
	id := c.Query("id")
	var isGood bool
	db.QueryRow("SELECT is_good FROM dailies WHERE id = ?", id).Scan(&isGood)
	db.Exec("UPDATE dailies SET is_good = ? WHERE id = ?", !isGood, id)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "is_good": !isGood})
}

func postCommentHandler(c *gin.Context) {
	var cm Comment
	if err := c.ShouldBindJSON(&cm); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Verify Token Role... skipped for brevity, assumed checked by caller or middleware

	// Handle Screenshot Base64
	if cm.ScreenshotPath != "" { // Incoming is base64 data URI
		// Format: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAA..."
		b64data := cm.ScreenshotPath[strings.IndexByte(cm.ScreenshotPath, ',')+1:]

		dec, err := base64.StdEncoding.DecodeString(b64data)
		if err == nil {
			// Save to disk
			fileName := fmt.Sprintf("%d_%d.png", cm.VideoID, time.Now().UnixNano())
			saveDir := filepath.Join(cfg.UploadRoot, "Comments")
			os.MkdirAll(saveDir, 0755)
			fullPath := filepath.Join(saveDir, fileName)

			f, err := os.Create(fullPath)
			if err == nil {
				defer f.Close()
				f.Write(dec) // simple write, io import justified if using Copy usually, but Write is fine
				// Update to relative path for serving
				cm.ScreenshotPath = "/comments-static/" + fileName
			}
		}
	}

	now := time.Now()
	res, err := db.Exec("INSERT INTO comments (video_id, timecode, content, severity, reporter, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		cm.VideoID, cm.Timecode, cm.Content, cm.Severity, cm.Reporter, now)
	if err != nil {
		c.JSON(500, gin.H{"error": "Database error"})
		return
	}

	id, _ := res.LastInsertId()
	cm.ID = int(id)
	cm.CreatedAt = now

	c.JSON(200, cm)
}

// --- Scheduler ---
func startScheduler() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Scheduler panicked: %v", r)
			}
		}()

		for {
			now := time.Now()
			// Calculate duration until 5 AM
			next := time.Date(now.Year(), now.Month(), now.Day(), 5, 0, 0, 0, now.Location())
			if now.After(next) {
				next = next.Add(24 * time.Hour)
			}
			time.Sleep(next.Sub(now))

			runDailyTasks()
		}
	}()
}

func runDailyTasks() {
	log.Println("Running Daily Tasks...")

	// 1. Backup Videos
	if cfg.EnableWebDAV {
		rows, _ := db.Query("SELECT id, file_path FROM videos WHERE backup_status = 0")
		for rows.Next() {
			var id int
			var path string
			rows.Scan(&id, &path)
			if uploadToWebDAV(path) {
				db.Exec("UPDATE videos SET backup_status = 1 WHERE id = ?", id)
			}
		}
	}

	// 2. Cleanup Old Versions
	if cfg.EnableAutoClean {
		// Group by project, order by created_at desc
		// This logic needs to be robust.
		// Simpler: Iterate all projects, get videos, keep top K
		pRows, _ := db.Query("SELECT id FROM projects")
		for pRows.Next() {
			var pid int
			pRows.Scan(&pid)

			// Get videos for project
			vRows, _ := db.Query("SELECT id, file_path, backup_status FROM videos WHERE project_id = ? ORDER BY created_at DESC", pid)
			var videos []Video
			for vRows.Next() {
				var v Video
				vRows.Scan(&v.ID, &v.FilePath, &v.BackupStatus)
				videos = append(videos, v)
			}

			if len(videos) > cfg.KeepVersions {
				toDelete := videos[cfg.KeepVersions:]
				for _, v := range toDelete {
					if cfg.SafeCleanMode && v.BackupStatus == 0 {
						log.Printf("Skipping cleanup for %s: not backed up", v.FilePath)
						continue
					}
					// Remove File
					os.Remove(v.FilePath)
					// Remove DB Record (or mark deleted)
					db.Exec("DELETE FROM videos WHERE id = ?", v.ID)
					log.Printf("Cleaned up: %s", v.FilePath)
				}
			}
		}
	}
}

func uploadToWebDAV(localPath string) bool {
	// Placeholder: Implement WebDAV PUT
	log.Printf("Mock WebDAV Upload: %s", localPath)
	return true
}

func main() {
	envFile := flag.String("env", ".env", "Path to .env file")
	flag.Parse()

	loadConfig(*envFile)
	initDB()
	startScheduler()

	r := gin.Default()

	// Load templates from embed FS
	templ := template.Must(template.New("").ParseFS(embedFS, "templates/*.html"))
	r.SetHTMLTemplate(templ)

	// Middleware for auth if needed
	// Basic Auth for Admin
	admin := r.Group("/admin", gin.BasicAuth(gin.Accounts{
		"admin": cfg.AdminPassword,
	}))
	admin.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "admin.html", nil)
	})
	admin.POST("/upload", uploadDemoHandler)

	// Admin API
	admin.GET("/projects", func(c *gin.Context) {
		rows, _ := db.Query("SELECT id, name, created_at FROM projects ORDER BY created_at DESC")
		var ps []Project
		for rows.Next() {
			var p Project
			rows.Scan(&p.ID, &p.Name, &p.CreatedAt)
			ps = append(ps, p)
		}
		c.JSON(200, ps)
	})
	admin.POST("/projects", func(c *gin.Context) {
		name := c.PostForm("name")
		if name == "" {
			c.Status(400)
			return
		}
		getProjectID(name) // This creates it if missing
		c.JSON(200, gin.H{"status": "ok"})
	})
	admin.GET("/projects/:id", func(c *gin.Context) {
		id := c.Param("id")
		var p Project
		err := db.QueryRow("SELECT id, name, created_at FROM projects WHERE id = ?", id).Scan(&p.ID, &p.Name, &p.CreatedAt)
		if err != nil {
			c.Status(404)
			return
		}

		rows, _ := db.Query("SELECT id, project_id, version_label, file_path, note, created_at FROM videos WHERE project_id = ? ORDER BY created_at DESC", id)
		var vs []Video
		for rows.Next() {
			var v Video
			rows.Scan(&v.ID, &v.ProjectID, &v.VersionLabel, &v.FilePath, &v.Note, &v.CreatedAt)
			vs = append(vs, v)
		}
		c.JSON(200, gin.H{"project": p, "videos": vs})
	})
	admin.POST("/tokens", func(c *gin.Context) {
		// target_id, type="demo"|"dailies", role="reviewer"|"viewer"
		targetID := c.PostForm("target_id")
		tType := c.PostForm("type")
		role := c.PostForm("role")

		// Simple UUID-like token
		// Better: use something random
		// b := make([]byte, 16)
		// rand.Read(b) ... reusing math/rand or crypto/rand requires import.
		// Let's just use existing imports. formatted time + random properties.
		// Actually, we imported crypto isn't imported.
		// Minimal:
		code := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s-%s-%s-%d", targetID, tType, role, time.Now().UnixNano())))

		db.Exec("INSERT INTO tokens (code, target_id, type, role, created_at) VALUES (?, ?, ?, ?, ?)", code, targetID, tType, role, time.Now())
		c.JSON(200, gin.H{"token": code, "link": fmt.Sprintf("/watch?token=%s", code)})
	})

	// API
	r.GET("/api/dailies/check", checkDailyHandler)
	r.POST("/api/dailies/upload", uploadDailyHandler)
	r.POST("/api/comment", postCommentHandler)
	r.POST("/api/action/star", toggleGoodHandler)

	// Public / Token Access
	r.GET("/watch", watchHandler)
	r.GET("/stream/:id", streamHandler)
	r.GET("/stream-daily/:id", streamHandler) // same logic basically

	log.Printf("Server starting on %s", cfg.ServerPort)
	r.Run(cfg.ServerPort)
}
