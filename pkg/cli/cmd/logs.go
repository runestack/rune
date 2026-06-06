package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/utils"
	"github.com/spf13/cobra"
)

// Valid output formats
const (
	OutputFormatText = "text"
	OutputFormatJSON = "json"
	OutputFormatRaw  = "raw"
)

type logsOptions struct {
	cmdOptions

	follow         bool
	tail           int
	since          time.Time
	until          time.Time
	sinceStr       string
	untilStr       string
	pattern        string
	showTimestamps bool
	showPrefix     bool
	noColor        bool
	outputFormat   string

	// initStep, when non-empty, switches `rune logs` into init-step
	// mode (RUNE-121 S6): instead of streaming container/process logs
	// we walk all instances of the service and print their
	// InitStepState rows for the named step. Real log streaming for
	// init steps will land in a follow-up once the log subsystem can
	// resolve InitStepState.LogRef.
	initStep string
}

func newLogsCmd() *cobra.Command {
	opts := &logsOptions{}
	cmd := &cobra.Command{
		Use:   "logs (SERVICE_NAME | INSTANCE_NAME | TYPE/NAME)",
		Short: "Show logs for a service or instance",
		Long: `Display logs for Rune services and instances.

This command allows you to view the logs of your services, helping
with debugging, monitoring, and understanding service behavior.

Examples:
  # Stream logs from a service by name
  rune logs api

  # View logs from a specific instance by name
  rune logs api-instance-123

  # Use the explicit TYPE/NAME format
  rune logs service/api

  # Show only the last 50 lines of logs
  rune logs api --tail=50

  # Show logs from the last 10 minutes
  rune logs api --since=10m

  # Show logs from the last 10 minutes until 5 minutes ago
  rune logs api --since=10m --until=5m

  # Filter logs containing the word "error"
  rune logs api --pattern=error

  # Show timestamps in the output
  rune logs api --timestamps

  # Show resource type, service, and instance prefixes
  rune logs api --prefix

  # Output logs in JSON format for machine processing
  rune logs api --output=json`,
		Aliases: []string{"log"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.namespace = effectiveCmdNS(opts.namespace)
			return runLogs(cmd, args, opts)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Local flags for the logs command
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "", "Namespace of the service")
	cmd.Flags().BoolVarP(&opts.follow, "follow", "f", false, "Stream logs in real-time")
	cmd.Flags().IntVarP(&opts.tail, "tail", "t", 100, "Number of recent log lines to show (use 0 for all available)")
	cmd.Flags().StringVar(&opts.sinceStr, "since", "", "Show logs since timestamp (e.g., '5m', '2h', '2023-01-01T10:00:00Z')")
	cmd.Flags().StringVar(&opts.untilStr, "until", "", "Show logs until timestamp (e.g., '5m', '2h', '2023-01-01T10:00:00Z')")
	cmd.Flags().StringVarP(&opts.pattern, "pattern", "p", "", "Filter logs by pattern")
	cmd.Flags().BoolVar(&opts.showTimestamps, "timestamps", false, "Show timestamps in the output")
	cmd.Flags().BoolVar(&opts.showPrefix, "prefix", false, "Show resource type, service and instance prefixes in log output")
	cmd.Flags().BoolVar(&opts.noColor, "no-color", false, "Disable colorized output")
	cmd.Flags().StringVarP(&opts.outputFormat, "output", "o", OutputFormatText, "Output format: text, json, or raw")
	cmd.Flags().StringVar(&opts.initStep, "init-step", "", "Show init step state for the named step on the service (RUNE-121)")

	// API client flags
	cmd.Flags().StringVar(&opts.addressOverride, "api-server", "", "Address of the API server")

	return cmd

}

func init() { rootCmd.AddCommand(newLogsCmd()) }

// runLogs is the main entry point for the logs command
func runLogs(cmd *cobra.Command, args []string, opts *logsOptions) error {
	resourceArg := args[0]

	// Configure logs options
	opts, err := parseLogsOptions(opts)
	if err != nil {
		return err
	}

	// Create API client
	apiClient, err := createAPIClient(&opts.cmdOptions)
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w", err)
	}
	defer apiClient.Close()

	// RUNE-121 S6: --init-step shortcuts the streaming path and prints
	// per-instance InitStepState rows for the named step instead.
	if opts.initStep != "" {
		return printInitStepLogs(apiClient, opts, resourceArg)
	}

	// Set up context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle Ctrl+C gracefully
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Fprintln(os.Stderr, "\nReceived interrupt, shutting down...")
		cancel()
	}()

	// History path (RUNE-200 / RuneSight): on the non-follow path, query the
	// native ObserveService for persisted history when observability is
	// enabled. Falls through to the live ephemeral stream when disabled,
	// unreachable, or in follow mode. No new verb.
	if !opts.follow {
		handled, err := tryObserveHistory(ctx, apiClient, resourceArg, opts)
		if handled {
			return err
		}
	}

	return streamLogs(ctx, apiClient, resourceArg, opts)
}

// parseLogsOptions parses and validates command line flags into TraceOptions
func parseLogsOptions(opts *logsOptions) (*logsOptions, error) {
	// Validate output format
	switch opts.outputFormat {
	case OutputFormatText, OutputFormatJSON, OutputFormatRaw:
		// Valid format
	default:
		return nil, fmt.Errorf("invalid output format: %s (must be text, json, or raw)", opts.outputFormat)
	}

	// Parse since time if provided
	if opts.sinceStr != "" {
		since, err := parseSinceTime(opts.sinceStr)
		if err != nil {
			return nil, fmt.Errorf("invalid since time: %w", err)
		}
		opts.since = since
	}

	// Parse until time if provided
	if opts.untilStr != "" {
		// If "until" is a relative time, interpret it as time ago from now
		until, err := parseSinceTime(opts.untilStr)
		if err != nil {
			return nil, fmt.Errorf("invalid until time: %w", err)
		}
		opts.until = until
	}

	return opts, nil
}

// parseSinceTime parses a human-friendly time string into a time.Time
func parseSinceTime(value string) (time.Time, error) {
	// Try as duration (e.g., "5m", "2h")
	if duration, err := time.ParseDuration(value); err == nil {
		return time.Now().Add(-duration), nil
	}

	// Try common timestamp formats
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}

	for _, layout := range formats {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("unrecognized time format: %s", value)
}

// streamLogs streams logs from all instances of a service
func streamLogs(ctx context.Context, apiClient *client.Client, targetName string, opts *logsOptions) error {
	// Create a log client directly from the API client
	logClient := client.NewLogClient(apiClient)

	// Create stream using the convenience method
	stream, err := logClient.StreamLogs(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to log service: %w", err)
	}

	// Create log request for service
	logRequest := &generated.LogRequest{
		ResourceTarget: targetName,
		Namespace:      opts.namespace,
		Follow:         opts.follow,
		Tail:           utils.ToInt32NonNegative(opts.tail),
		Timestamps:     opts.showTimestamps,
	}

	// Add common parameters to request
	addCommonRequestParams(logRequest, opts)

	// Send the initial request
	if err := stream.Send(logRequest); err != nil {
		return fmt.Errorf("failed to send log request: %w", err)
	}

	// We don't send any further client-to-server messages today, so half-close
	// the send side. This lets the server's client-watch goroutine observe EOF
	// promptly when we cancel, instead of blocking on Recv forever.
	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("failed to close send stream: %w", err)
	}

	if !opts.follow {
		return handleNonStreamingLogs(ctx, stream, opts)
	}
	return handleStreamingLogs(ctx, stream, opts)
}

// tryObserveHistory queries the native observability (RuneSight) ObserveService
// for persisted log history when it is enabled. It is used only on the
// non-follow path (history is a point-in-time query, not a tail). Returns
// (handled=false, nil) when observability is disabled or unreachable so the
// caller falls back to the live ephemeral stream; returns (true, err) once it
// has taken ownership of output. No new verb — same `rune logs` surface
// (plan §4.6).
func tryObserveHistory(ctx context.Context, apiClient *client.Client, targetName string, opts *logsOptions) (bool, error) {
	obs := client.NewObserveClient(apiClient)
	caps, err := obs.Capabilities(ctx)
	if err != nil || caps == nil || !caps.GetEnabled() {
		// Disabled or unreachable — fall back to the live stream.
		return false, nil
	}

	// Build a Core-tier LogQL query from the resource target. `rune logs api`
	// selects the service stream; the embedded store also accepts instance
	// matches, but the CLI resolves by service name for parity with the live
	// path's service-level default.
	logql := fmt.Sprintf("{service=%q}", targetName)
	if opts.pattern != "" {
		logql += fmt.Sprintf(" |= %q", opts.pattern)
	}

	rows, err := obs.QueryLogs(ctx, logql, opts.namespace, opts.since, opts.until, opts.tail, false /* newest-first */)
	if err != nil {
		// Surface the error rather than silently degrading: the operator
		// explicitly has observability on and asked for history.
		return true, fmt.Errorf("query log history: %w", err)
	}

	// Print oldest-first for readability (query returned newest-first).
	for i := len(rows) - 1; i >= 0; i-- {
		processObserveRow(rows[i], targetName, opts)
	}
	return true, nil
}

// processObserveRow renders a persisted LogRow through the same formatting path
// as live log responses by adapting it into a generated.LogResponse.
func processObserveRow(row *generated.LogRow, serviceName string, opts *logsOptions) {
	if row == nil {
		return
	}
	resp := &generated.LogResponse{
		ServiceName:  serviceName,
		InstanceId:   row.GetLabels()["instance"],
		InstanceName: row.GetLabels()["instance"],
		Timestamp:    row.GetTimestamp(),
		Content:      row.GetLine(),
		Stream:       row.GetStream(),
		LogLevel:     row.GetLevel(),
	}
	if !shouldProcessLog(resp, opts) {
		return
	}
	if err := processLogResponse(resp, opts); err != nil {
		fmt.Fprintf(os.Stderr, "Error processing log: %v\n", err)
	}
}

// addCommonRequestParams adds common parameters to the log request
func addCommonRequestParams(logRequest *generated.LogRequest, opts *logsOptions) {
	// Add since time if specified
	if !opts.since.IsZero() {
		logRequest.Since = opts.since.Format(time.RFC3339)
	}

	// Add until time if specified
	if !opts.until.IsZero() {
		logRequest.Until = opts.until.Format(time.RFC3339)
	}

	// Add filter if specified
	if opts.pattern != "" {
		logRequest.Filter = opts.pattern
	}
}

// handleNonStreamingLogs collects logs until the server signals EOF (or the
// user cancels), keeps at most opts.tail of them (across all instances), and
// prints them in receive order. tail<=0 means "no client-side cap": rely on
// the server-applied tail.
func handleNonStreamingLogs(ctx context.Context, stream generated.LogService_StreamLogsClient, opts *logsOptions) error {
	type recvResult struct {
		resp *generated.LogResponse
		err  error
	}
	recvCh := make(chan recvResult, 1)
	go func() {
		defer close(recvCh)
		for {
			r, e := stream.Recv()
			recvCh <- recvResult{resp: r, err: e}
			if e != nil {
				return
			}
		}
	}()

	useRing := opts.tail > 0
	var ring []*generated.LogResponse
	if useRing {
		ring = make([]*generated.LogResponse, 0, opts.tail)
	}
	var all []*generated.LogResponse

collect:
	for {
		select {
		case <-ctx.Done():
			break collect
		case res, ok := <-recvCh:
			if !ok {
				break collect
			}
			if res.err != nil {
				if res.err == io.EOF {
					break collect
				}
				if ctx.Err() != nil {
					break collect
				}
				return fmt.Errorf("error receiving logs: %w", res.err)
			}
			if !shouldProcessLog(res.resp, opts) {
				continue
			}
			if useRing {
				if len(ring) < opts.tail {
					ring = append(ring, res.resp)
				} else {
					copy(ring, ring[1:])
					ring[len(ring)-1] = res.resp
				}
			} else {
				all = append(all, res.resp)
			}
		}
	}

	logsToProcess := all
	if useRing {
		logsToProcess = ring
	}
	for _, resp := range logsToProcess {
		if resp == nil {
			continue
		}
		if err := processLogResponse(resp, opts); err != nil {
			fmt.Fprintf(os.Stderr, "Error processing log: %v\n", err)
		}
	}

	return nil
}

// handleStreamingLogs handles logs in streaming (follow) mode. Logs are
// printed inline as they arrive. The function returns when the server closes
// the stream, an error occurs, or the user cancels via ctx (Ctrl+C). When ctx
// is cancelled, the gRPC stream's Recv returns an error and the goroutine
// exits cleanly.
func handleStreamingLogs(ctx context.Context, stream generated.LogService_StreamLogsClient, opts *logsOptions) error {
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("error receiving logs: %w", err)
		}
		if !shouldProcessLog(resp, opts) {
			continue
		}
		if err := processLogResponse(resp, opts); err != nil {
			fmt.Fprintf(os.Stderr, "Error processing log: %v\n", err)
		}
	}
}

// shouldProcessLog determines if a log should be processed based on filters
func shouldProcessLog(resp *generated.LogResponse, opts *logsOptions) bool {
	// Apply pattern filter if specified
	if opts.pattern != "" && !strings.Contains(strings.ToLower(resp.Content), strings.ToLower(opts.pattern)) {
		return false
	}

	// Skip logs outside the time range if Until is specified
	if !opts.until.IsZero() {
		if logTime, err := time.Parse(time.RFC3339, resp.Timestamp); err == nil {
			if logTime.After(opts.until) {
				return false
			}
		}
	}

	return true
}

// processLogResponse processes and displays a log response based on the output format.
// Filtering is done upstream in shouldProcessLog; this function only formats output.
func processLogResponse(resp *generated.LogResponse, opts *logsOptions) error {
	logLevel := resp.LogLevel
	if logLevel == "" {
		logLevel = "info"
		if strings.Contains(strings.ToLower(resp.Content), "error") {
			logLevel = "error"
		}
	}

	// Normalize embedded line breaks so each response prints on a single line.
	content := resp.Content
	content = strings.ReplaceAll(content, "\r", " ")
	content = strings.ReplaceAll(content, "\n", " ")
	content = strings.Join(strings.Fields(content), " ")

	timeColor := color.New(color.FgCyan)
	nameColor := color.New(color.FgGreen, color.Bold)
	errorColor := color.New(color.FgRed)
	infoColor := color.New(color.FgWhite)
	if opts.noColor {
		timeColor.DisableColor()
		nameColor.DisableColor()
		errorColor.DisableColor()
		infoColor.DisableColor()
	}

	// Build a display timestamp; keep an RFC3339 form for JSON output.
	const displayLayout = "2006-01-02T15:04:05.000Z07:00"
	displayTimestamp := resp.Timestamp
	rfcTimestamp := resp.Timestamp
	if rfcTimestamp == "" {
		now := time.Now().UTC()
		rfcTimestamp = now.Format(time.RFC3339Nano)
		displayTimestamp = now.Format(displayLayout)
	} else if t, err := time.Parse(time.RFC3339, resp.Timestamp); err == nil {
		displayTimestamp = t.Format(displayLayout)
		rfcTimestamp = t.UTC().Format(time.RFC3339Nano)
	}

	var prefix string
	if opts.showPrefix {
		var prefixParts []string
		if resp.ServiceName != "" {
			prefixParts = append(prefixParts, resp.ServiceName)
		}
		if resp.InstanceName != "" {
			prefixParts = append(prefixParts, resp.InstanceName)
		}
		if len(prefixParts) > 0 {
			prefix = nameColor.Sprint(strings.Join(prefixParts, "/")) + " "
		}
	}

	switch opts.outputFormat {
	case OutputFormatJSON:
		jsonEntry := map[string]string{
			"timestamp": rfcTimestamp,
			"service":   resp.ServiceName,
			"instance":  resp.InstanceId,
			"content":   content,
			"level":     logLevel,
			"stream":    resp.Stream,
		}
		jsonBytes, err := json.Marshal(jsonEntry)
		if err != nil {
			return err
		}
		fmt.Println(string(jsonBytes))

	case OutputFormatRaw:
		fmt.Println(content)

	case OutputFormatText:
		fallthrough
	default:
		var formattedContent string
		if logLevel == "error" {
			formattedContent = errorColor.Sprint(content)
		} else {
			formattedContent = infoColor.Sprint(content)
		}
		formattedContent = strings.TrimSpace(formattedContent)

		if opts.showTimestamps {
			fmt.Printf("%s | %s%s\n", timeColor.Sprint(displayTimestamp), prefix, formattedContent)
		} else {
			fmt.Printf("%s%s\n", prefix, formattedContent)
		}
	}

	return nil
}
