package rtcp_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	rtcp "example.com/gostrtcpsdk/src"
)

func ExampleClient_Run() {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // The host may stop before a connection is established.
	client, err := rtcp.NewClient(rtcp.Config{Server: "127.0.0.1:1080", Bind: "127.0.0.1:8080"}, "127.0.0.1:80")
	if err != nil {
		panic(err)
	}
	err = client.Run(ctx)
	fmt.Println(errors.Is(err, context.Canceled))
	// Output: true
}

// Build and run an independent module, not another package inside this module.
func TestExternalGoModule(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Dir(root) // Tests run in the src package; replace targets the module root.
	dir := t.TempDir()
	mod := "module example.com/sdk-consumer\n\ngo 1.23\n\nrequire example.com/gostrtcpsdk v0.0.0\nreplace example.com/gostrtcpsdk => " + strconv.Quote(filepath.ToSlash(root)) + "\n"
	program := `package main
import (
 "context"
 "errors"
 "fmt"
 rtcp "example.com/gostrtcpsdk/src"
)
func main() {
 ctx, cancel := context.WithCancel(context.Background())
 cancel()
 client, err := rtcp.NewClient(rtcp.Config{Server:"127.0.0.1:1080", Bind:"127.0.0.1:8080"}, "127.0.0.1:80")
 if err != nil { panic(err) }
 if err := client.UpdateTarget("localhost:81"); err != nil { panic(err) }
 err = client.Run(ctx)
 if !errors.Is(err, context.Canceled) { panic(err) }
 fmt.Println("external SDK consumer: OK")
}
`
	for name, content := range map[string]string{"go.mod": mod, "main.go": program} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, args := range [][]string{{"mod", "tidy"}, {"run", "."}} {
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("external module %v: %v\n%s", args, err, out)
		}
		if args[0] == "run" && !strings.Contains(string(out), "external SDK consumer: OK") {
			t.Fatalf("unexpected output: %s", out)
		}
	}
}
