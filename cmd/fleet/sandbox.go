package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"fleet/internal/client"
	"fleet/internal/config"
	"fleet/internal/sandbox"
)

func newSandboxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Manage the Docker image sandboxed agents run in",
	}
	cmd.AddCommand(newSandboxBuildCmd(), newSandboxDockerfileCmd())
	return cmd
}

func newSandboxBuildCmd() *cobra.Command {
	var tag string
	var pull bool
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build the default agent image (Claude Code, Codex, git, gh) on this machine",
		Long: `Build the image sandboxed agents run in, from fleet's built-in
Dockerfile ("fleet sandbox dockerfile" prints it). Run it on the server.
--pull rebuilds from scratch with the latest base image and agent CLIs,
which is how sandboxed agents are updated.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !client.IsLocal(host) {
				return errors.New("sandbox build runs docker on this machine; run it on the server")
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if tag == "" {
				tag = cfg.Sandbox.ImageOrDefault()
			}
			d := &sandbox.Docker{Binary: cfg.Sandbox.DockerOrDefault()}
			if err := d.Build(cmd.Context(), tag, sandbox.Dockerfile, pull, cmd.ErrOrStderr()); err != nil {
				return fmt.Errorf("docker build: %w", err)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "built %s\n", tag)
			return nil
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "", "image tag (default: [sandbox] image, or "+config.DefaultSandboxImage+")")
	cmd.Flags().BoolVar(&pull, "pull", false, "pull the latest base image and rebuild without cache")
	return cmd
}

func newSandboxDockerfileCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dockerfile",
		Short: "Print the built-in Dockerfile (to extend it with your own tools)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write(sandbox.Dockerfile)
			return err
		},
	}
}
