package mcps

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// 用当前测试程序作为临时 MCP 服务，真正经过 OS 子进程与 stdin/stdout，
// 覆盖管道桩无法验证的启动、环境变量、EOF 退出和进程回收。
func TestStdioProcessRoundTrip(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := StartStdio(ctx, ServerConfig{
		Name: "process-test", Command: executable, Args: []string{"-test.run=^TestStdioHelperProcess$"},
		Env: map[string]string{"HERMES_TEST_MCP_PROCESS": "1"}, InitTimeoutSeconds: 3, CallTimeoutSeconds: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	tools, err := client.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("真实子进程列工具失败：%+v, %v", tools, err)
	}
	result, err := client.CallTool(ctx, "echo", json.RawMessage(`{"text":"真实进程调用成功"}`))
	if err != nil || result.IsError || len(result.Content) != 1 || result.Content[0].Text != "真实进程调用成功" {
		t.Fatalf("真实 stdio 调用失败：%+v, %v", result, err)
	}
	if err = client.Close(); err != nil {
		t.Fatalf("服务未正常退出：%v", err)
	}
}

func TestStdioHelperProcess(t *testing.T) {
	if os.Getenv("HERMES_TEST_MCP_PROCESS") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
			Params struct {
				Arguments struct {
					Text string `json:"text"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || request.ID == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "process-fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "echo", "description": "测试回显", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]string{"type": "string"}}}}}}
		case "tools/call":
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": request.Params.Arguments.Text}}}
		default:
			os.Exit(2)
		}
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		fmt.Fprintln(os.Stdout, string(data))
	}
	// 不让测试框架的 PASS 文本污染协议 stdout。
	os.Exit(0)
}
