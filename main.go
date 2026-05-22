package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"aicodeagent/cache"
	"aicodeagent/database"
	"aicodeagent/models"
	"aicodeagent/websocket"

	"github.com/chromedp/chromedp"
	"github.com/gin-gonic/gin"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ========== Prometheus 指标 ==========
var (
	tasksProcessed = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "agent_tasks_total", Help: "Total tasks processed"},
		[]string{"status"},
	)
	taskDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{Name: "agent_task_duration_seconds", Help: "Task duration"},
	)
)

func init() {
	prometheus.MustRegister(tasksProcessed)
	prometheus.MustRegister(taskDuration)
}

// ========== 跨域中间件 ==========
func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Origin, Content-Type, Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}

// ========== Prometheus 中间件 ==========
func PrometheusMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		taskDuration.Observe(time.Since(start).Seconds())
	}
}

// ========== 工具接口 ==========
type Tool interface {
	Name() string
	Description() string
	Execute(ctx context.Context, input string) (string, error)
}

// ---------- 代码生成工具 ----------
type CodeGenTool struct {
	ConversationID string
}

func (t CodeGenTool) Name() string        { return "code_generator" }
func (t CodeGenTool) Description() string { return "调用大模型生成代码" }
func (t CodeGenTool) Execute(ctx context.Context, input string) (string, error) {
	history := getConversationHistory(t.ConversationID)
	code := callZhipuAIWithHistory(input, history)
	if code == "" || strings.HasPrefix(code, "[ERROR]") || strings.HasPrefix(code, "AI returned empty") {
		return code, nil
	}
	return uploadCodeToMinIO(ctx, code)
}

func getConversationHistory(convID string) []map[string]string {
	if convID == "" {
		return nil
	}
	var tasks []models.Task
	database.DB.Where("conversation_id = ? AND status = ?", convID, "done").
		Order("created_at asc").Find(&tasks)
	var history []map[string]string
	for _, t := range tasks {
		history = append(history, map[string]string{"role": "user", "content": t.Input})
		history = append(history, map[string]string{"role": "assistant", "content": t.Result})
	}
	return history
}

func callZhipuAIWithHistory(prompt string, history []map[string]string) string {
	apiKey := os.Getenv("ZHIPU_API_KEY")
	if apiKey == "" {
		return "[ERROR] ZHIPU_API_KEY not set"
	}
	messages := []map[string]string{
		{"role": "system", "content": "你是一个专业的编程助手，擅长生成各类代码。如果用户追问或要求修改，请基于之前的对话内容进行回答。"},
	}
	messages = append(messages, history...)
	messages = append(messages, map[string]string{"role": "user", "content": prompt})

	reqBody := map[string]interface{}{
		"model":       "glm-4-flash",
		"messages":    messages,
		"temperature": 0.3,
	}
	data, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", "https://open.bigmodel.cn/api/paas/v4/chat/completions", bytes.NewBuffer(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          10,
			ForceAttemptHTTP2:     false,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				MaxVersion: tls.VersionTLS13,
			},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("AI 调用失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(body, &result)
	if choices, ok := result["choices"].([]interface{}); ok && len(choices) > 0 {
		if msg, ok := choices[0].(map[string]interface{})["message"].(map[string]interface{}); ok {
			if content, ok := msg["content"].(string); ok {
				return content
			}
		}
	}
	return "AI 返回为空"
}

// ---------- 搜索工具 ----------
type SearchTool struct {
	knowledge map[string]string
}

func NewSearchTool() *SearchTool {
	return &SearchTool{
		knowledge: map[string]string{
			"go":  "Go 是 Google 开发的编程语言，擅长高并发。",
			"gin": "Gin 是 Go 的高性能 Web 框架。",
		},
	}
}
func (t *SearchTool) Name() string        { return "search" }
func (t *SearchTool) Description() string { return "搜索本地知识库" }
func (t *SearchTool) Execute(ctx context.Context, input string) (string, error) {
	kw := strings.ToLower(input)
	for k, v := range t.knowledge {
		if strings.Contains(kw, k) {
			return v, nil
		}
	}
	return "未找到相关信息", nil
}

// ---------- 截图工具 ----------
type ScreenshotTool struct {
	ConversationID string
}

func (t ScreenshotTool) Name() string        { return "screenshot" }
func (t ScreenshotTool) Description() string { return "对 HTML 进行截图，返回图片 URL" }
func (t ScreenshotTool) Execute(ctx context.Context, input string) (string, error) {
	return takeScreenshot(ctx, input)
}

func takeScreenshot(ctx context.Context, html string) (string, error) {
	tmpFile, err := os.CreateTemp("", "preview-*.html")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.WriteString(html)
	tmpFile.Close()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
	)
	allocCtx, cancel := chromedp.NewExecAllocator(ctx, opts...)
	defer cancel()
	taskCtx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	var buf []byte
	if err := chromedp.Run(taskCtx,
		chromedp.Navigate("file://"+tmpFile.Name()),
		chromedp.Sleep(2*time.Second),
		chromedp.FullScreenshot(&buf, 90),
	); err != nil {
		return "", err
	}
	if minioClient == nil {
		return fmt.Sprintf("data:image/png;base64,%s", bytes.NewBuffer(buf).String()), nil
	}
	objectName := fmt.Sprintf("screenshots/%d.png", time.Now().UnixNano())
	return uploadFileToMinIO(ctx, objectName, buf, "image/png")
}

// ---------- 打包工具 ----------
type PackageTool struct {
	ConversationID string
}

func (t PackageTool) Name() string        { return "package" }
func (t PackageTool) Description() string { return "打包代码为 ZIP，返回下载链接" }
func (t PackageTool) Execute(ctx context.Context, input string) (string, error) {
	backend := `package main
import "github.com/gin-gonic/gin"
func main() {
	r := gin.Default()
	r.GET("/ping", func(c *gin.Context) { c.JSON(200, gin.H{"msg":"pong"}) })
	r.Run(":8080")
}`
	frontend := `<html><body><h1>Hello AI Platform</h1></body></html>`
	if minioClient == nil {
		return "MinIO 未连接，无法上传代码包", nil
	}
	return uploadCodePackage(ctx, backend, frontend)
}

// ========== MinIO 存储 ==========
var minioClient *minio.Client
var bucketName = "ai-platform"

func InitMinIO() {
	endpoint := os.Getenv("MINIO_ENDPOINT")
	if endpoint == "" {
		log.Println("[WARN] MINIO_ENDPOINT 未设置，截图/打包功能将受限")
		return
	}
	accessKey := os.Getenv("MINIO_ACCESS_KEY")
	if accessKey == "" {
		accessKey = "minioadmin"
	}
	secretKey := os.Getenv("MINIO_SECRET_KEY")
	if secretKey == "" {
		secretKey = "minioadmin"
	}
	var err error
	minioClient, err = minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,
	})
	if err != nil {
		log.Printf("[WARN] MinIO 连接失败: %v", err)
		minioClient = nil
		return
	}
	ctx := context.Background()
	exists, _ := minioClient.BucketExists(ctx, bucketName)
	if !exists {
		minioClient.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
	}
	log.Println("[OK] MinIO 已连接")
}

func uploadCodeToMinIO(ctx context.Context, code string) (string, error) {
	if minioClient == nil {
		return code + "\n\n[WARN] MinIO 未连接", nil
	}
	contentType := "text/plain"
	fileExt := ".txt"
	if strings.Contains(code, "package main") {
		contentType = "text/x-go"
		fileExt = ".go"
	} else if strings.Contains(code, "<html") {
		contentType = "text/html"
		fileExt = ".html"
	}
	objectName := fmt.Sprintf("codes/%d%s", time.Now().UnixNano(), fileExt)
	_, err := minioClient.PutObject(ctx, bucketName, objectName, bytes.NewReader([]byte(code)), int64(len(code)), minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return code + "\n\n[ERROR] 上传失败", nil
	}
	reqParams := make(url.Values)
	presignedURL, _ := minioClient.PresignedGetObject(ctx, bucketName, objectName, 7*24*time.Hour, reqParams)
	externalEndpoint := os.Getenv("MINIO_EXTERNAL_ENDPOINT")
	if externalEndpoint != "" {
		presignedURL.Host = externalEndpoint
		presignedURL.Scheme = "http"
	}
	return fmt.Sprintf("%s\n\n[DOWNLOAD_BUTTON:%s]", code, presignedURL.String()), nil
}

func uploadFileToMinIO(ctx context.Context, objectName string, data []byte, contentType string) (string, error) {
	_, err := minioClient.PutObject(ctx, bucketName, objectName, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return "", err
	}
	url, _ := minioClient.PresignedGetObject(ctx, bucketName, objectName, 7*24*time.Hour, nil)
	return url.String(), nil
}

func uploadCodePackage(ctx context.Context, backend, frontend string) (string, error) {
	buf := new(bytes.Buffer)
	zipWriter := zip.NewWriter(buf)
	f1, _ := zipWriter.Create("backend/main.go")
	f1.Write([]byte(backend))
	f2, _ := zipWriter.Create("frontend/index.html")
	f2.Write([]byte(frontend))
	zipWriter.Close()
	objectName := fmt.Sprintf("packages/%d.zip", time.Now().UnixNano())
	return uploadFileToMinIO(ctx, objectName, buf.Bytes(), "application/zip")
}

// ========== 工作流引擎 ==========
type Workflow struct {
	Name  string
	Nodes map[string]*WorkflowNode
	State map[string]any
	mu    sync.RWMutex
}

type WorkflowNode struct {
	Name      string
	DependsOn []string
	Action    func(ctx context.Context, state map[string]any) (map[string]any, error)
}

func NewWorkflow(name string) *Workflow {
	return &Workflow{Name: name, Nodes: make(map[string]*WorkflowNode), State: make(map[string]any)}
}
func (w *Workflow) AddNode(node *WorkflowNode) { w.Nodes[node.Name] = node }
func (w *Workflow) Execute(ctx context.Context) error {
	for _, node := range w.Nodes {
		state, err := node.Action(ctx, w.State)
		if err != nil {
			return err
		}
		w.mu.Lock()
		for k, v := range state {
			w.State[k] = v
		}
		w.mu.Unlock()
	}
	return nil
}

// ========== Agent 核心 ==========
type Agent struct {
	tools          map[string]Tool
	tasksProcessed *prometheus.CounterVec
	taskDuration   prometheus.Histogram
}

func NewAgent(tp *prometheus.CounterVec, td prometheus.Histogram) *Agent {
	a := &Agent{
		tools:          make(map[string]Tool),
		tasksProcessed: tp,
		taskDuration:   td,
	}
	a.Register(CodeGenTool{})
	a.Register(NewSearchTool())
	a.Register(ScreenshotTool{})
	a.Register(PackageTool{})
	return a
}
func (a *Agent) Register(t Tool) { a.tools[t.Name()] = t }

func (a *Agent) DecideTool(input string) string {
	lower := strings.ToLower(input)
	if strings.Contains(lower, "截图") || strings.Contains(lower, "screenshot") ||
		strings.Contains(lower, "capture") || strings.Contains(lower, "预览") {
		return "screenshot"
	}
	if strings.Contains(lower, "打包") || strings.Contains(lower, "package") ||
		strings.Contains(lower, "下载") || strings.Contains(lower, "bundle") {
		return "workflow"
	}
	actionWords := []string{
		"写", "生成", "实现", "创建", "开发", "编写", "帮我", "输出",
		"改成", "修改", "加上", "去掉", "优化", "重构",
		"做个", "弄个", "搞个", "整个", "来个", "新建", "构建",
		"做", "弄", "搞", "整", "来", "设计", "制作", "定制",
		"write", "generate", "create", "implement", "develop", "code",
		"build", "make", "do", "help", "output",
	}
	techWords := []string{
		"gin", "go", "接口", "api", "代码", "sql", "vue", "html", "css",
		"c++", "python", "java", "javascript", "typescript", "react",
		"node", "django", "flask", "spring", "rust", "typescript",
		"hello world", "rest", "grpc", "gorm", "docker", "k8s",
	}
	hasAction := false
	for _, w := range actionWords {
		if strings.Contains(lower, w) {
			hasAction = true
			break
		}
	}
	hasTech := false
	for _, w := range techWords {
		if strings.Contains(lower, w) {
			hasTech = true
			break
		}
	}
	if hasAction && hasTech {
		return "code_generator"
	}
	return "search"
}

func (a *Agent) ExecuteWithContext(ctx context.Context, convID, input string) (string, error) {
	start := time.Now()
	defer func() { a.taskDuration.Observe(time.Since(start).Seconds()) }()

	toolName := a.DecideTool(input)
	if toolName == "workflow" {
		return a.executeWorkflow(ctx, convID)
	}
	var tool Tool
	switch toolName {
	case "code_generator":
		tool = CodeGenTool{ConversationID: convID}
	case "screenshot":
		tool = ScreenshotTool{ConversationID: convID}
	case "package":
		tool = PackageTool{ConversationID: convID}
	case "search":
		tool = NewSearchTool()
	default:
		return "未找到合适的工具", nil
	}
	result, err := tool.Execute(ctx, input)
	if err != nil {
		a.tasksProcessed.WithLabelValues("failed").Inc()
		return "", err
	}
	a.tasksProcessed.WithLabelValues("success").Inc()
	return result, nil
}

func (a *Agent) executeWorkflow(ctx context.Context, convID string) (string, error) {
	w := NewWorkflow("fullstack")
	w.AddNode(&WorkflowNode{
		Name: "gen-backend",
		Action: func(ctx context.Context, state map[string]any) (map[string]any, error) {
			history := getConversationHistory(convID)
			code := callZhipuAIWithHistory("用 Gin 生成一个简单的 RESTful API", history)
			state["backend"] = code
			return state, nil
		},
	})
	w.AddNode(&WorkflowNode{
		Name:      "gen-frontend",
		DependsOn: []string{"gen-backend"},
		Action: func(ctx context.Context, state map[string]any) (map[string]any, error) {
			history := getConversationHistory(convID)
			code := callZhipuAIWithHistory("用 Vue 3 生成一个数据展示页面", history)
			state["frontend"] = code
			return state, nil
		},
	})
	w.AddNode(&WorkflowNode{
		Name:      "package",
		DependsOn: []string{"gen-frontend"},
		Action: func(ctx context.Context, state map[string]any) (map[string]any, error) {
			backend := state["backend"].(string)
			frontend := state["frontend"].(string)
			url, _ := uploadCodePackage(ctx, backend, frontend)
			state["package_url"] = url
			return state, nil
		},
	})
	w.AddNode(&WorkflowNode{
		Name:      "screenshot",
		DependsOn: []string{"gen-frontend"},
		Action: func(ctx context.Context, state map[string]any) (map[string]any, error) {
			html := state["frontend"].(string)
			url, _ := takeScreenshot(ctx, html)
			state["screenshot_url"] = url
			return state, nil
		},
	})
	if err := w.Execute(ctx); err != nil {
		return "", err
	}
	return fmt.Sprintf("工作流完成。\n下载链接：%s\n截图预览：%s", w.State["package_url"], w.State["screenshot_url"]), nil
}

// ========== MCP 服务 ==========
func getStringArg(request mcp.CallToolRequest, key string) string {
	args, ok := request.Params.Arguments.(map[string]interface{})
	if !ok {
		return ""
	}
	v, _ := args[key].(string)
	return v
}

func startMCPServer(r *gin.Engine, ag *Agent) {
	s := server.NewMCPServer(
		"AI Code Agent",
		"1.0.0",
		server.WithToolCapabilities(true),
	)

	// 注册代码生成工具
	codeGenTool := mcp.NewTool("code_generator",
		mcp.WithDescription("调用大模型生成代码"),
		mcp.WithString("prompt", mcp.Required(), mcp.Description("代码需求描述")),
	)
	s.AddTool(codeGenTool, server.ToolHandlerFunc(func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		prompt := getStringArg(request, "prompt")
		result, err := ag.ExecuteWithContext(ctx, "", prompt)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(result), nil
	}))

	// 注册搜索工具
	searchTool := mcp.NewTool("search",
		mcp.WithDescription("搜索本地知识库"),
		mcp.WithString("query", mcp.Required(), mcp.Description("搜索关键词")),
	)
	s.AddTool(searchTool, server.ToolHandlerFunc(func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query := getStringArg(request, "query")
		result, err := ag.ExecuteWithContext(ctx, "", query)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(result), nil
	}))

	// 注册截图工具
	screenshotTool := mcp.NewTool("screenshot",
		mcp.WithDescription("对 HTML 进行截图，返回图片 URL"),
		mcp.WithString("html", mcp.Required(), mcp.Description("要截图的 HTML 内容")),
	)
	s.AddTool(screenshotTool, server.ToolHandlerFunc(func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		html := getStringArg(request, "html")
		result, err := ag.ExecuteWithContext(ctx, "", html)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(result), nil
	}))

	// 注册打包工具
	packageTool := mcp.NewTool("package",
		mcp.WithDescription("打包代码为 ZIP，返回下载链接"),
		mcp.WithString("input", mcp.Required(), mcp.Description("打包输入")),
	)
	s.AddTool(packageTool, server.ToolHandlerFunc(func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		input := getStringArg(request, "input")
		result, err := ag.ExecuteWithContext(ctx, "", input)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(result), nil
	}))

	sseServer := server.NewSSEServer(s, server.WithStaticBasePath("/mcp"))
	r.Any("/mcp/sse", func(c *gin.Context) {
		sseServer.ServeHTTP(c.Writer, c.Request)
	})
	r.Any("/mcp/message", func(c *gin.Context) {
		sseServer.ServeHTTP(c.Writer, c.Request)
	})
	log.Println("[MCP] 服务已启动，端点: /mcp/sse 和 /mcp/message")
}

// ========== 全局变量与 Worker ==========
var (
	ag        *Agent
	wsManager *websocket.Manager
	taskQueue = make(chan models.Task, 100)
	wg        sync.WaitGroup
)

func startWorker() {
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range taskQueue {
				database.DB.Model(&task).Update("status", "processing")
				cache.SetTaskCache(task)

				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				result, err := ag.ExecuteWithContext(ctx, task.ConversationID, task.Input)
				cancel()

				if err != nil {
					task.Status = "failed"
					task.Result = err.Error()
				} else {
					task.Status = "done"
					task.Result = result
				}
				database.DB.Save(&task)
				cache.SetTaskCache(task)
				wsManager.BroadcastTaskDone(task.ID, task.Result)
			}
		}()
	}
}

// ========== HTTP 处理器 ==========
func handleSubmit(c *gin.Context) {
	input := c.Query("task")
	convID := c.Query("conversation_id")

	if input == "" {
		c.String(400, "请提供 task 参数")
		return
	}

	if convID == "" {
		conv := models.Conversation{
			ID:        fmt.Sprintf("conv_%d", time.Now().UnixNano()),
			Title:     truncateString(input, 30),
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
		database.DB.Create(&conv)
		convID = conv.ID
	} else {
		database.DB.Model(&models.Conversation{}).Where("id = ?", convID).Update("updated_at", time.Now())
	}

	task := models.Task{
		ID:             fmt.Sprintf("%d", time.Now().UnixNano()),
		ConversationID: convID,
		Input:          input,
		Status:         "waiting",
	}
	database.DB.Create(&task)
	cache.SetTaskCache(task)
	taskQueue <- task

	c.JSON(200, gin.H{"task_id": task.ID, "conversation_id": convID})
}

func handleResult(c *gin.Context) {
	id := c.Query("id")
	task, err := cache.GetTask(id)
	if err != nil {
		c.JSON(404, gin.H{"error": "任务不存在"})
		return
	}
	c.JSON(200, task)
}

func handleTasks(c *gin.Context) {
	var tasks []models.Task
	database.DB.Order("created_at desc").Find(&tasks)
	c.JSON(200, tasks)
}

func handleDeleteTask(c *gin.Context) {
	id := c.Param("id")
	database.DB.Delete(&models.Task{}, "id = ?", id)
	cache.DeleteTaskCache(id)
	c.JSON(200, gin.H{"msg": "deleted"})
}

func handleCreateConversation(c *gin.Context) {
	conv := models.Conversation{
		ID:        fmt.Sprintf("conv_%d", time.Now().UnixNano()),
		Title:     "新对话",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	database.DB.Create(&conv)
	c.JSON(200, conv)
}

func handleListConversations(c *gin.Context) {
	var convs []models.Conversation
	database.DB.Order("updated_at desc").Find(&convs)
	c.JSON(200, convs)
}

func handleGetConversationMessages(c *gin.Context) {
	convID := c.Param("id")
	var tasks []models.Task
	database.DB.Where("conversation_id = ?", convID).Order("created_at asc").Find(&tasks)

	messages := []map[string]interface{}{}
	for _, t := range tasks {
		if t.Status == "done" || t.Status == "failed" {
			messages = append(messages, map[string]interface{}{"role": "user", "content": t.Input})
			messages = append(messages, map[string]interface{}{"role": "assistant", "content": t.Result})
		}
	}
	c.JSON(200, messages)
}

func handleDeleteConversation(c *gin.Context) {
	convID := c.Param("id")
	database.DB.Where("conversation_id = ?", convID).Delete(&models.Task{})
	database.DB.Delete(&models.Conversation{}, "id = ?", convID)
	cache.DeleteTaskCache(convID)
	c.JSON(200, gin.H{"msg": "deleted"})
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// ========== 主函数 ==========
func main() {
	database.InitDB()
	cache.InitRedis()
	InitMinIO()

	ag = NewAgent(tasksProcessed, taskDuration)
	wsManager = websocket.NewManager()
	go wsManager.Run()
	startWorker()

	r := gin.Default()
	r.Use(CORSMiddleware())
	r.Use(PrometheusMiddleware())
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))
	r.Static("/frontend", "./frontend")
	r.GET("/ws", func(c *gin.Context) { wsManager.ServeWS(c.Writer, c.Request) })

	r.POST("/submit", handleSubmit)
	r.GET("/result", handleResult)
	r.GET("/tasks", handleTasks)
	r.DELETE("/task/:id", handleDeleteTask)

	r.POST("/conversations", handleCreateConversation)
	r.GET("/conversations", handleListConversations)
	r.GET("/conversations/:id/messages", handleGetConversationMessages)
	r.DELETE("/conversations/:id", handleDeleteConversation)

	startMCPServer(r, ag)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Println("Shutting down...")
		close(taskQueue)
		wg.Wait()
		os.Exit(0)
	}()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Server started on :%s", port)
	r.Run(":" + port)
}
