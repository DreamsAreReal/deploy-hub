package scan

import (
	"context"
	"fmt"
	"os"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	ImageArchive  = "image.tar"
	reasonImage   = "the container image has vulnerabilities, secrets or misconfigurations"
	reasonNoImage = "no image was provided for an app that ships one"
)

func Image(ctx context.Context, ws gate.Workspace) gate.Outcome {
	if ws.Image == "" {
		return gate.Fail(reasonNoImage)
	}
	archive, err := imageArchive(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	return trivy(ctx, ws, reasonImage, "image", "--scanners", "vuln,secret,misconfig", "--image-config-scanners", "misconfig,secret", "--input", archive)
}

func imageArchive(ctx context.Context, ws gate.Workspace) (string, error) {
	path := ws.Report(ImageArchive)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if _, err := ws.Run(ctx, DockerBinary, "save", "-o", path, ws.Image); err != nil {
		return "", fmt.Errorf("docker save %s: %w: %s", ws.Image, err, gate.Tail(procexec.Output(err)))
	}
	return path, nil
}
