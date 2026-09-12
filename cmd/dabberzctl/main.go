// Command dabberzctl drives the dabberz control plane from a terminal.
//
// The web UI is the intended v1 interface; this exists for setup, for
// scripting, and for the things that are easier to say than to click.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/dabbers/devex/internal/secrets"
)

// version is set at build time.
var version = "dev"

const defaultEndpoint = "http://127.0.0.1:8080"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "dabberzctl:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		usage(out)
		return errors.New("a command is required")
	}

	command, rest := args[0], args[1:]
	switch command {
	case "help", "-h", "--help":
		usage(out)
		return nil
	case "version", "--version":
		fmt.Fprintln(out, "dabberzctl", version)
		return nil
	case "keygen":
		return keygen(out)
	}

	client := &client{endpoint: endpoint()}
	switch command {
	case "status":
		return client.get(out, "/v1/capacity")
	case "repos":
		return client.repos(out, rest)
	case "tasks":
		return client.tasks(out, rest)
	case "forks":
		return client.forks(out, rest)
	case "events":
		return client.events(out, rest)
	case "secrets":
		return client.secrets(out, rest)
	default:
		usage(out)
		return fmt.Errorf("unknown command %q", command)
	}
}

func usage(out io.Writer) {
	fmt.Fprint(out, `dabberzctl drives the dabberz control plane.

Setup:
  keygen                            generate a secrets master key
  status                            machine capacity and both queues

Repos:
  repos list
  repos add <name> <remote-url>
  repos discover <repo-id> <path>   infer the projects in a repo from a file listing
  repos projects <repo-id>

Tasks:
  tasks list [repo-id]
  tasks new <repo-id> <request>     create a task and plan it
  tasks show <task-id>
  tasks answer <task-id> <question-id> <answer>
  tasks approve <task-id>           start the workstreams
  tasks cancel <task-id> [reason]

Forks:
  forks show <fork-id>
  forks resolve <fork-id> <response>  answer an escalation and resume

Activity:
  events [task-id]
  events follow [task-id]           stream the activity feed

Secrets (scoped to a repo, inherited by every fork under it):
  secrets list <repo-id>
  secrets set <repo-id> <NAME> <value>
  secrets rm <repo-id> <NAME>

The control plane endpoint comes from DABBERZ_ENDPOINT (default `+defaultEndpoint+`).
`)
}

func endpoint() string {
	if v := os.Getenv("DABBERZ_ENDPOINT"); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return defaultEndpoint
}

// keygen prints a fresh master key for the secrets store.
func keygen(out io.Writer) error {
	key, err := secrets.GenerateKey()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\n\nSet this in the daemon's environment and keep a copy somewhere safe:\n  export DABBERZ_MASTER_KEY=%s\n\nStored secrets cannot be recovered without it.\n", key, key)
	return nil
}

// client talks to the control plane.
type client struct {
	endpoint string
}

func (c *client) do(method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, c.endpoint+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the control plane at %s: %w", c.endpoint, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(detail)))
	}
	return resp, nil
}

// get fetches a path and pretty-prints the JSON.
func (c *client) get(out io.Writer, path string) error {
	resp, err := c.do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return prettyPrint(out, resp.Body)
}

func (c *client) post(out io.Writer, path string, body any) error {
	resp, err := c.do(http.MethodPost, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		fmt.Fprintln(out, "ok")
		return nil
	}
	return prettyPrint(out, resp.Body)
}

func (c *client) repos(out io.Writer, args []string) error {
	if len(args) == 0 {
		return c.get(out, "/v1/repos")
	}
	switch args[0] {
	case "list":
		return c.get(out, "/v1/repos")
	case "add":
		if len(args) < 3 {
			return errors.New("usage: repos add <name> <remote-url>")
		}
		return c.post(out, "/v1/repos", map[string]string{"name": args[1], "remote_url": args[2]})
	case "projects":
		if len(args) < 2 {
			return errors.New("usage: repos projects <repo-id>")
		}
		return c.get(out, "/v1/repos/"+args[1]+"/projects")
	case "discover":
		if len(args) < 3 {
			return errors.New("usage: repos discover <repo-id> <path-to-checkout>")
		}
		tree, err := listTree(args[2])
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "inspecting %d files: %s\n\n", len(tree), treeSummary(tree))
		return c.post(out, "/v1/repos/"+args[1]+"/discover", map[string]any{"tree": tree})
	default:
		return fmt.Errorf("unknown repos subcommand %q", args[0])
	}
}

func (c *client) tasks(out io.Writer, args []string) error {
	if len(args) == 0 {
		return c.get(out, "/v1/tasks")
	}
	switch args[0] {
	case "list":
		path := "/v1/tasks"
		if len(args) > 1 {
			path += "?repo=" + args[1]
		}
		return c.get(out, path)
	case "new":
		if len(args) < 3 {
			return errors.New("usage: tasks new <repo-id> <request>")
		}
		return c.post(out, "/v1/tasks", map[string]any{
			"repo_id": args[1],
			"request": strings.Join(args[2:], " "),
			// Plan straight away: the planning checkpoint is where the user
			// picks the work up again.
			"plan": true,
		})
	case "show":
		if len(args) < 2 {
			return errors.New("usage: tasks show <task-id>")
		}
		return c.get(out, "/v1/tasks/"+args[1])
	case "answer":
		if len(args) < 4 {
			return errors.New("usage: tasks answer <task-id> <question-id> <answer>")
		}
		return c.post(out, "/v1/tasks/"+args[1]+"/answers", map[string]any{
			"answers": map[string]string{args[2]: strings.Join(args[3:], " ")},
		})
	case "approve":
		if len(args) < 2 {
			return errors.New("usage: tasks approve <task-id>")
		}
		return c.post(out, "/v1/tasks/"+args[1]+"/approve", nil)
	case "cancel":
		if len(args) < 2 {
			return errors.New("usage: tasks cancel <task-id> [reason]")
		}
		return c.post(out, "/v1/tasks/"+args[1]+"/cancel", map[string]string{
			"reason": strings.Join(args[2:], " "),
		})
	default:
		return fmt.Errorf("unknown tasks subcommand %q", args[0])
	}
}

func (c *client) forks(out io.Writer, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: forks show|resolve <fork-id> [...]")
	}
	switch args[0] {
	case "show":
		return c.get(out, "/v1/forks/"+args[1])
	case "resolve":
		if len(args) < 3 {
			return errors.New("usage: forks resolve <fork-id> <response>")
		}
		return c.post(out, "/v1/forks/"+args[1]+"/resolve", map[string]any{
			"response": strings.Join(args[2:], " "),
		})
	default:
		return fmt.Errorf("unknown forks subcommand %q", args[0])
	}
}

func (c *client) events(out io.Writer, args []string) error {
	follow := len(args) > 0 && args[0] == "follow"
	if follow {
		args = args[1:]
	}

	query := ""
	if len(args) > 0 {
		query = "?task=" + args[0]
	}
	if !follow {
		return c.get(out, "/v1/events"+query)
	}

	resp, err := c.do(http.MethodGet, "/v1/events/stream"+query, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Server-sent events: print each data line as it arrives.
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			for _, line := range strings.Split(string(buf[:n]), "\n") {
				if payload, ok := strings.CutPrefix(line, "data: "); ok {
					fmt.Fprintln(out, payload)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (c *client) secrets(out io.Writer, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: secrets list|set|rm <repo-id> [...]")
	}
	repo := args[1]
	switch args[0] {
	case "list":
		return c.get(out, "/v1/repos/"+repo+"/secrets")
	case "set":
		if len(args) < 4 {
			return errors.New("usage: secrets set <repo-id> <NAME> <value>")
		}
		resp, err := c.do(http.MethodPut, "/v1/repos/"+repo+"/secrets/"+args[2],
			map[string]string{"value": strings.Join(args[3:], " ")})
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		fmt.Fprintln(out, "ok")
		return nil
	case "rm":
		if len(args) < 3 {
			return errors.New("usage: secrets rm <repo-id> <NAME>")
		}
		resp, err := c.do(http.MethodDelete, "/v1/repos/"+repo+"/secrets/"+args[2], nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		fmt.Fprintln(out, "ok")
		return nil
	default:
		return fmt.Errorf("unknown secrets subcommand %q", args[0])
	}
}

// prettyPrint re-indents a JSON response for reading in a terminal.
func prettyPrint(out io.Writer, body io.Reader) error {
	raw, err := io.ReadAll(io.LimitReader(body, 32<<20))
	if err != nil {
		return err
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, "", "  "); err != nil {
		// Not JSON. Showing the body as-is is more useful than refusing to
		// print a response the user asked for.
		fmt.Fprintln(out, strings.TrimSpace(string(raw)))
		return nil //nolint:nilerr // the raw body is the intended output here
	}
	fmt.Fprintln(out, indented.String())
	return nil
}
