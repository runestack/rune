package cmd

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/version"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// versionInfo is the structured form printed by `rune version -o json|yaml`.
type versionInfo struct {
	Client versionPart    `json:"client" yaml:"client"`
	Server *versionPart   `json:"server,omitempty" yaml:"server,omitempty"`
	Note   string         `json:"note,omitempty" yaml:"note,omitempty"`
}

type versionPart struct {
	Version   string `json:"version" yaml:"version"`
	Commit    string `json:"commit,omitempty" yaml:"commit,omitempty"`
	BuildTime string `json:"buildTime,omitempty" yaml:"buildTime,omitempty"`
	GoVersion string `json:"goVersion,omitempty" yaml:"goVersion,omitempty"`
	OS        string `json:"os,omitempty" yaml:"os,omitempty"`
	Arch      string `json:"arch,omitempty" yaml:"arch,omitempty"`
}

var (
	versionClientOnly bool
	versionOutput     string
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show client and server version information",
	Long: `Display version information for the local rune CLI and, when reachable,
the connected runed server. Use --client to skip the server probe.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		info := versionInfo{Client: clientVersionPart()}

		if !versionClientOnly {
			sp, note := fetchServerVersion()
			info.Server = sp
			info.Note = note
		}

		switch versionOutput {
		case "json":
			b, err := json.MarshalIndent(info, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		case "yaml":
			b, err := yaml.Marshal(info)
			if err != nil {
				return err
			}
			fmt.Print(string(b))
			return nil
		case "", "text":
			printVersionText(info)
			return nil
		default:
			return fmt.Errorf("unsupported output format: %s (supported: text, json, yaml)", versionOutput)
		}
	},
}

func clientVersionPart() versionPart {
	return versionPart{
		Version:   version.Version,
		Commit:    shortCommit(version.Commit),
		BuildTime: version.BuildTime,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}

// fetchServerVersion best-effort probes the configured server. It never
// returns an error so `rune version` works offline.
func fetchServerVersion() (*versionPart, string) {
	api, err := newAPIClient("", "")
	if err != nil {
		return nil, "Server: unreachable (run 'rune login' to configure a context)"
	}
	defer api.Close()

	hc := generated.NewHealthServiceClient(api.Conn())
	ctx, cancel := api.Context()
	defer cancel()
	resp, err := hc.GetServerVersion(ctx, &generated.GetServerVersionRequest{})
	if err != nil || resp == nil {
		return nil, "Server: unreachable or does not support GetServerVersion"
	}
	return &versionPart{
		Version:   resp.Version,
		Commit:    shortCommit(resp.Commit),
		BuildTime: resp.BuildTime,
		GoVersion: resp.GoVersion,
		OS:        resp.Os,
		Arch:      resp.Arch,
	}, ""
}

func printVersionText(info versionInfo) {
	fmt.Println("Client:")
	printPart(info.Client)
	if info.Server != nil {
		fmt.Println("Server:")
		printPart(*info.Server)
	} else if info.Note != "" {
		fmt.Println(info.Note)
	}
}

func printPart(p versionPart) {
	fmt.Printf("  Version:    %s\n", p.Version)
	if p.Commit != "" {
		fmt.Printf("  Commit:     %s\n", p.Commit)
	}
	if p.BuildTime != "" {
		fmt.Printf("  BuildTime:  %s\n", p.BuildTime)
	}
	if p.GoVersion != "" {
		fmt.Printf("  GoVersion:  %s\n", p.GoVersion)
	}
	if p.OS != "" || p.Arch != "" {
		fmt.Printf("  Platform:   %s/%s\n", p.OS, p.Arch)
	}
}

func shortCommit(c string) string {
	if len(c) > 8 {
		return c[:8]
	}
	return c
}

func init() {
	versionCmd.Flags().BoolVar(&versionClientOnly, "client", false, "Show client version only; skip the server probe")
	versionCmd.Flags().StringVarP(&versionOutput, "output", "o", "text", "Output format (text, json, yaml)")
	rootCmd.AddCommand(versionCmd)
}
