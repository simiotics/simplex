package main

import (
	"bufio"
	"context"
	"database/sql"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"testing"

	dockerTypes "github.com/docker/docker/api/types"

	"github.com/simiotics/simplex/builds"
	"github.com/simiotics/simplex/components"
	"github.com/simiotics/simplex/executions"
	"github.com/simiotics/simplex/state"
)

func TestSingleComponent(t *testing.T) {
	stateDir, err := ioutil.TempDir("", "simplex-TestSingleComponent-")
	if err != nil {
		t.Fatalf("Could not create directory to hold Simplex state: %s", err.Error())
	}
	os.RemoveAll(stateDir)

	err = state.Init(stateDir)
	if err != nil {
		t.Fatalf("Error initializing Simplex state directory: %s", err.Error())
	}
	defer os.RemoveAll(stateDir)

	stateDBPath := path.Join(stateDir, state.DBFileName)
	db, err := sql.Open("sqlite3", stateDBPath)
	if err != nil {
		t.Fatal("Error opening state database file")
	}
	defer db.Close()

	componentID := "test-component"
	componentPath := "examples/single-task"
	specificationPath := "examples/single-task/component.json"
	component, err := components.AddComponent(db, componentID, components.Task, componentPath, specificationPath)
	if err != nil {
		t.Fatalf("Error registering component: %s", err.Error())
	}

	if component.ID != componentID {
		t.Fatalf("Unexpected component ID: expected=%s, actual=%s", componentID, component.ID)
	}
	if component.ComponentType != components.Task {
		t.Fatalf("Unexpected component type: expected=%s, actual=%s", components.Task, component.ComponentType)
	}
	if component.ComponentPath != componentPath {
		t.Fatalf("Unexpected component path: expected=%s, actual=%s", componentPath, component.ComponentPath)
	}
	if component.SpecificationPath != specificationPath {
		t.Fatalf("Unexpected component path: expected=%s, actual=%s", specificationPath, component.SpecificationPath)
	}

	dockerClient := generateDockerClient()
	ctx := context.Background()

	build, err := builds.CreateBuild(ctx, db, dockerClient, ioutil.Discard, component.ID)
	if err != nil {
		t.Fatalf("Error building image for component: %s", err.Error())
	}
	if build.ComponentID != component.ID {
		t.Fatalf("Unexpected component ID on build: expected=%s, actual=%s", component.ID, build.ComponentID)
	}

	imageInfo, _, err := dockerClient.ImageInspectWithRaw(ctx, build.ID)
	if err != nil {
		t.Fatalf("Could not inspect image with tag: %s", build.ID)
	}
	defer dockerClient.ImageRemove(ctx, imageInfo.ID, dockerTypes.ImageRemoveOptions{Force: true, PruneChildren: true})

	buildTags := map[string]bool{}
	for _, tag := range imageInfo.RepoTags {
		buildTags[tag] = true
	}

	if _, ok := buildTags[build.ID]; !ok {
		t.Fatalf("Expected tag (%s) was not registered against docker daemon", build.ID)
	}

	tagParts := strings.Split(build.ID, ":")
	if len(tagParts) > 1 {
		tagParts[len(tagParts)-1] = "latest"
	}
	latestTag := strings.Join(tagParts, ":")
	if _, ok := buildTags[latestTag]; !ok {
		t.Fatalf("Expected tag (%s) was not registered against docker daemon", latestTag)
	}

	mounts := map[string]string{}
	specFile, err := os.Open(specificationPath)
	if err != nil {
		t.Fatalf("Error opening specification file (%s): %s", specificationPath, err.Error())
	}
	specification, err := components.ReadSingleSpecification(specFile)
	if err != nil {
		t.Fatalf("Error parsing specification (%s): %s", specificationPath, err.Error())
	}
	for _, mountpoint := range specification.Run.Mountpoints {
		sourceFile, err := ioutil.TempFile("", "")
		sourceFile.Close()
		if err != nil {
			t.Fatalf("Error creating temporary file to mount onto container path %s: %s", mountpoint.Mountpoint, err.Error())
		}
		mounts[sourceFile.Name()] = mountpoint.Mountpoint
		defer os.Remove(sourceFile.Name())
	}

	execution, err := executions.Execute(ctx, db, dockerClient, build.ID, "", mounts)
	if err != nil {
		t.Fatalf("Error executing build (%s): %s", build.ID, err.Error())
	}
	exitCode, err := dockerClient.ContainerWait(ctx, execution.ID)
	if err != nil {
		t.Fatalf("Error waiting for container (ID: %s) to exit: %s", execution.ID, err.Error())
	}
	if exitCode != 0 {
		t.Fatalf("Received non-zero exit code (%d) from container (ID: %s)", exitCode, execution.ID)
	}
	defer dockerClient.ContainerRemove(ctx, execution.ID, dockerTypes.ContainerRemoveOptions{})

	inverseMounts := map[string]string{}
	for source, target := range mounts {
		inverseMounts[target] = source
	}
	outfile, err := os.Open(inverseMounts["/simplex/outputs/outputs.txt"])
	if err != nil {
		t.Fatalf("Could not open output file (%s): %s", inverseMounts["/simplex/outputs/outputs.txt"], err.Error())
	}
	defer outfile.Close()
	scanner := bufio.NewScanner(outfile)
	more := scanner.Scan()
	if !more {
		t.Fatal("Not enough lines in output file")
	}
	line := scanner.Text()
	expectedLine := specification.Run.Env["MY_ENV"]
	if line != expectedLine {
		t.Fatalf("Incorrect value in output file: expected=\"%s\", actual=\"%s\"", expectedLine, line)
	}

	terminating := 0
	for scanner.Scan() {
		terminating++
		line = scanner.Text()
		if line != "" {
			t.Fatalf("Got unexpected non-empty line from output file: %s", line)
		}
	}

	if terminating > 1 {
		t.Fatalf("Too many terminating newlines in output file: %d", terminating)
	}

	// TODO(nkashy1): Implement execution state management and add those functions into this test
}