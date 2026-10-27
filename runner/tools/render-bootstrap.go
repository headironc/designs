//go:build ignore

// Run inside the verified LXD provider v0.1.5 archive with -mod=vendor.
// This checks the native consumer using synthetic identity only; it creates no VM.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/cloudbase/garm-provider-common/cloudconfig"
	"github.com/cloudbase/garm-provider-common/params"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: render-bootstrap TEMPLATE OUTPUT")
		os.Exit(1)
	}
	if err := render(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func render(sourcePath, outputPath string) error {
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	specs, err := json.Marshal(map[string]any{"disable_updates": true, "runner_install_template": source})
	if err != nil {
		return err
	}
	bootstrap := params.BootstrapInstance{
		Name: "nucleiron-bootstrap-synthetic", MetadataURL: "https://10.203.64.1:9443/api/v1/metadata",
		CallbackURL: "https://10.203.64.1:9443/api/v1/callbacks", InstanceToken: "synthetic-instance-token-never-log",
		JitConfigEnabled: true, OSType: params.Linux, ExtraSpecs: specs,
		UserDataOptions: params.UserDataOptions{DisableUpdatesOnBoot: true},
	}
	filename, url := "synthetic-runner.tar.gz", "https://example.invalid/synthetic-runner.tar.gz"
	script, err := cloudconfig.GetRunnerInstallScript(bootstrap, params.RunnerApplicationDownload{Filename: &filename, DownloadURL: &url}, bootstrap.Name)
	if err != nil {
		return err
	}
	config, err := cloudconfig.GetCloudInitConfig(bootstrap, script)
	if err != nil {
		return err
	}
	for path, data := range map[string][]byte{outputPath: script, outputPath + ".cloud-init": []byte(config)} {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(data)
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	fmt.Println("native template and cloud-init rendered with synthetic identity")
	return nil
}
