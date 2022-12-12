package stacks

import (
	"context"
	"errors"
	"os"
	"regexp"

	"github.com/rootsami/terradrift/pkg/config"
	log "github.com/sirupsen/logrus"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hc-install/product"
	"github.com/hashicorp/hc-install/releases"
	"github.com/hashicorp/terraform-config-inspect/tfconfig"
)

// install should recieve a terraform version and return the execution path
func install(stack config.Stack, workspace string) (string, error) {

	v, err := detectTFVersion(workspace + stack.Path)
	if err != nil {
		return "", err
	}

	// To make sure returned value doesn't include '>='
	tfver := regexp.MustCompile(`[^a-zA-Z0-9. ]+`).ReplaceAllString(v, "")

	execPathDir := os.TempDir() + tfver
	execPath := execPathDir + "/terraform"

	if _, err := os.Stat(execPath); errors.Is(err, os.ErrNotExist) {
		os.MkdirAll(execPathDir, os.ModePerm)
		installer := &releases.ExactVersion{
			Product:    product.Terraform,
			Version:    version.Must(version.NewVersion(tfver)),
			InstallDir: execPathDir,
		}

		log.WithFields(log.Fields{"stack": stack.Name, "version": tfver}).Info("Installing Terraform...")

		execPath, err := installer.Install(context.Background())
		if err != nil {
			return "", err
		}
		return execPath, nil

	} else {

		log.WithFields(log.Fields{"stack": stack.Name, "version": tfver}).Info("Skipping download, Terraform binary found...")

		return execPath, nil
	}

}

// Detect terraform version based on its definition in tf files
func detectTFVersion(path string) (string, error) {

	module, err := tfconfig.LoadModule(path)
	if err != nil {
		return "", err
	}
	tfversion := module.RequiredCore[0]

	return tfversion, nil

}
