// Copyright 2020 Matthew Holt
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package xcaddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/caddyserver/xcaddy/internal/utils"
	"github.com/google/shlex"
)

func (b Builder) newEnvironment(ctx context.Context) (*environment, error) {
	// assume Caddy v2 if no semantic version is provided
	caddyModulePath := defaultCaddyModulePath
	if !strings.HasPrefix(b.CaddyVersion, "v") || !strings.Contains(b.CaddyVersion, ".") {
		caddyModulePath += "/v2"
	}
	caddyModulePath, err := versionedModulePath(caddyModulePath, b.CaddyVersion)
	if err != nil {
		return nil, err
	}

	// clean up any SIV-incompatible module paths real quick
	for i, p := range b.Plugins {
		b.Plugins[i].PackagePath, err = versionedModulePath(p.PackagePath, p.Version)
		if err != nil {
			return nil, err
		}
	}

	// create the context for the main module template
	tplCtx := goModTemplateContext{
		CaddyModule: caddyModulePath,
	}
	for _, p := range b.Plugins {
		tplCtx.Plugins = append(tplCtx.Plugins, p.PackagePath)
	}

	// evaluate the template for the main module
	var buf bytes.Buffer
	templateName := "main"
	templateContent := mainModuleTemplate
	if b.BuildPlugin {
		templateName = "plugin"
		templateContent = pluginModuleTemplate
	}
	tpl, err := template.New(templateName).Parse(templateContent)
	if err != nil {
		return nil, err
	}
	err = tpl.Execute(&buf, tplCtx)
	if err != nil {
		return nil, err
	}

	// create the folder in which the build environment will operate
	tempFolder, err := newTempFolder()
	if err != nil {
		return nil, err
	}
	defer func() {
		if b.SkipCleanup {
			return
		}
		if err != nil {
			err2 := os.RemoveAll(tempFolder)
			if err2 != nil {
				err = fmt.Errorf("%w; additionally, cleaning up folder: %v", err, err2)
			}
		}
	}()
	log.Printf("[INFO] Temporary folder: %s", tempFolder)

	// write the main module file to temporary folder
	mainPath := filepath.Join(tempFolder, "main.go")
	log.Printf("[INFO] Writing main module: %s\n%s", mainPath, buf.Bytes())
	err = os.WriteFile(mainPath, buf.Bytes(), 0o644)
	if err != nil {
		return nil, err
	}

	if len(b.EmbedDirs) > 0 {
		for _, d := range b.EmbedDirs {
			err = copy(d.Dir, filepath.Join(tempFolder, "files", d.Name))
			if err != nil {
				return nil, err
			}
			_, err = os.Stat(d.Dir)
			if err != nil {
				return nil, fmt.Errorf("embed directory does not exist: %s", d.Dir)
			}
			log.Printf("[INFO] Embedding directory: %s", d.Dir)
			buf.Reset()
			tpl, err = template.New("embed").Parse(embeddedModuleTemplate)
			if err != nil {
				return nil, err
			}
			err = tpl.Execute(&buf, tplCtx)
			if err != nil {
				return nil, err
			}
			log.Printf("[INFO] Writing 'embedded' module: %s\n%s", mainPath, buf.Bytes())
			emedPath := filepath.Join(tempFolder, "embed.go")
			err = os.WriteFile(emedPath, buf.Bytes(), 0o644)
			if err != nil {
				return nil, err
			}
		}
	}

	env := &environment{
		caddyVersion:    b.CaddyVersion,
		plugins:         b.Plugins,
		caddyModulePath: caddyModulePath,
		tempFolder:      tempFolder,
		timeoutGoGet:    b.TimeoutGet,
		skipCleanup:     b.SkipCleanup,
		buildFlags:      b.BuildFlags,
		modFlags:        b.ModFlags,
		caddyBin:        b.CaddyBin,
		pluginDir:       b.PluginDir,
	}

	// initialize the go module
	log.Println("[INFO] Initializing Go module")
	modName := "caddy"
	if b.BuildPlugin && len(b.Plugins) > 0 {
		// use a unique module name for plugins so multiple plugins can be loaded
		modName = "caddyplugin_" + strings.NewReplacer("/", "_", ".", "_", "-", "_").Replace(b.Plugins[0].PackagePath)
	}
	cmd := env.newGoModCommand(ctx, "init")
	cmd.Args = append(cmd.Args, modName)
	err = env.runCommand(ctx, cmd)
	if err != nil {
		return nil, err
	}

	// specify module replacements before pinning versions
	replaced := make(map[string]string)
	for _, r := range b.Replacements {
		log.Printf("[INFO] Replace %s => %s", r.Old.String(), r.New.String())
		replaced[r.Old.String()] = r.New.String()
	}

	// check for early abort
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	// The timeout for the `go get` command may be different than `go build`,
	// so create a new context with the timeout for `go get`
	if env.timeoutGoGet > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), env.timeoutGoGet)
		defer cancel()
	}

	log.Println("[INFO] Pinning versions")
	binDeps := make(map[string]string)
	
	// files to extract versions from
	var extractFrom []string
	if env.caddyBin != "" {
		extractFrom = append(extractFrom, env.caddyBin)
	}
	if env.pluginDir != "" {
		files, err := os.ReadDir(env.pluginDir)
		if err != nil {
			return nil, fmt.Errorf("reading plugin directory: %v", err)
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			// normally Caddy plugins are .so files
			if strings.HasSuffix(f.Name(), ".so") {
				extractFrom = append(extractFrom, filepath.Join(env.pluginDir, f.Name()))
			}
		}
	}

	for _, file := range extractFrom {
		log.Printf("[INFO] Extracting dependency versions from %s", file)
		cmd := exec.Command(utils.GetGo(), "version", "-m", file)
		var out bytes.Buffer
		cmd.Stdout = &out
		err := cmd.Run()
		if err != nil {
			return nil, fmt.Errorf("getting version from %s: %v", file, err)
		}

		var extractedTags string
		var extractedTrimpath bool
		lines := strings.Split(out.String(), "\n")
		for i := 0; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			parts := strings.Fields(line)
			if len(parts) < 2 {
				continue
			}
			if parts[0] == "path" && len(parts) >= 3 && (parts[1] == defaultCaddyModulePath || strings.HasPrefix(parts[1], defaultCaddyModulePath+"/")) {
				// if CaddyVersion was not specified, we can use the one from the binary (only if it's the main caddy binary)
				if env.caddyVersion == "" && file == env.caddyBin {
					env.caddyVersion = parts[2]
				}
				continue
			}
			if parts[0] == "dep" && len(parts) >= 3 {
				mod, ver := parts[1], parts[2]
				
				// if we already have this dependency, check if versions match
				if existingVer, ok := binDeps[mod]; ok && existingVer != ver {
					log.Printf("[WARNING] Dependency version mismatch for %s: %s (from %s) vs %s (already seen). Using %s.", mod, ver, file, existingVer, existingVer)
					// For now, we keep the first one we saw. 
					// In a real Caddy build, it would resolve to the latest fitting version.
					// But since we are aligning with existing binaries, we have a conflict.
					continue
				}
				binDeps[mod] = ver

				// check if there's a replacement for this dependency on the next line
				replacement := ""
				if i+1 < len(lines) {
					nextLine := strings.TrimSpace(lines[i+1])
					if strings.HasPrefix(nextLine, "=>") {
						nextParts := strings.Fields(nextLine)
						if len(nextParts) >= 2 {
							replacement = nextParts[1]
							i++ // skip the replacement line in the next iteration
						}
					}
				}

				if replacement != "" {
					// only use replacement from binary if not already specified by user
					if _, ok := replaced[mod]; !ok {
						log.Printf("[INFO] Extracting replacement %s => %s (from %s)", mod, replacement, file)

						// if replacement is relative, resolve it relative to the binary's location
						if strings.HasPrefix(replacement, ".") {
							binAbs, err := filepath.Abs(file)
							if err == nil {
								replacement = filepath.Join(filepath.Dir(binAbs), replacement)
								replacement = filepath.Clean(replacement)
							}
						}
						replaced[mod] = replacement
					} else {
						// if it's already in 'replaced', it could be from user or from previous file
						// we only log if it was from user (which is already done or will be done)
						// log.Printf("[INFO] Using existing replacement for %s instead of %s from %s", mod, replacement, file)
					}
				}
				continue
			}
			// build flags are only extracted from the main Caddy binary
			if file == env.caddyBin && parts[0] == "build" && len(parts) >= 2 {
				for _, part := range parts[1:] {
					arg, val, hasVal := strings.Cut(part, "=")
					switch arg {
					case "-trimpath":
						if !hasVal || val == "true" {
							extractedTrimpath = true
						}
					case "-tags":
						if hasVal {
							extractedTags = val
						}
					}
				}
				// also handle the case where tags are a separate argument
				if parts[1] == "-tags" && len(parts) >= 3 {
					extractedTags = parts[2]
				}
			}
		}

		if extractedTrimpath {
			if !strings.Contains(env.buildFlags, "-trimpath") {
				if env.buildFlags != "" {
					env.buildFlags = "-trimpath " + env.buildFlags
				} else {
					env.buildFlags = "-trimpath"
				}
			}
		}
		if extractedTags != "" {
			if !strings.Contains(env.buildFlags, "-tags") {
				if env.buildFlags != "" {
					env.buildFlags = "-tags " + extractedTags + " " + env.buildFlags
				} else {
					env.buildFlags = "-tags " + extractedTags
				}
			}
		}
		if (extractedTrimpath || extractedTags != "") && file == env.caddyBin {
			log.Printf("[INFO] Using build flags from binary: %s", env.buildFlags)
		}
	}

	if len(replaced) > 0 {
		cmd := env.newGoModCommand(ctx, "edit")
		for o, n := range replaced {
			cmd.Args = append(cmd.Args, "-replace", fmt.Sprintf("%s=%s", o, n))
		}
		err := env.runCommand(ctx, cmd)
		if err != nil {
			return nil, err
		}
	}

	// fetch new modules and check if the latest or tagged version is viable
	// regardless if this is in reference to a local module, lexical submodule or minor semantic revision.
	for _, p := range b.Plugins {
		// also pass the Caddy version to prevent it from being upgraded
		err = env.execGoGet(ctx, p.PackagePath, p.Version, caddyModulePath, env.caddyVersion)
		if err != nil {
			return nil, err
		}
		// check for early abort
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}

	// if we have a reference binary, we now pin the versions of the modules
	// that are actually in the module graph to match the binary.
	if len(binDeps) > 0 {
		log.Println("[INFO] Aligning versions with Caddy binary")

		// First, pin Caddy itself to the version in the binary if not specified by user.
		// This sets the baseline for all Caddy-related dependencies.
		err = env.execGoGet(ctx, caddyModulePath, env.caddyVersion, "", "")
		if err != nil {
			return nil, err
		}

		// Get the current module graph after pinning Caddy and adding plugins.
		// We use -json to get both the module path and the currently selected version.
		cmd, err := env.newGoBuildCommand(ctx, "list", "-m", "-json", "all")
		if err != nil {
			return nil, err
		}
		var out bytes.Buffer
		cmd.Stdout = &out
		err = env.runCommand(ctx, cmd)
		if err != nil {
			return nil, err
		}

		type modInfo struct {
			Path    string
			Version string
		}
		currentMods := make(map[string]string)
		
		// go list -json all outputs multiple JSON objects
		dec := json.NewDecoder(&out)
		for dec.More() {
			var mi modInfo
			if err := dec.Decode(&mi); err != nil {
				break
			}
			currentMods[mi.Path] = mi.Version
		}

		for mod, ver := range binDeps {
			// Skip Caddy itself and its submodules
			if mod == caddyModulePath || strings.HasPrefix(mod, caddyModulePath+"/") {
				continue
			}

			// Skip the plugin we're building
			isPlugin := false
			for _, p := range env.plugins {
				if p.PackagePath == mod {
					isPlugin = true
					break
				}
			}
			if isPlugin {
				continue
			}

			// Check if this module is even in our current module graph
			currentVer, ok := currentMods[mod]
			if !ok {
				// Not in the graph, don't pin it (this is the key to speed!)
				continue
			}

			// If it's already at the version we want, skip pinning
			if currentVer == ver {
				continue
			}

			log.Printf("[INFO] Pinning %s to %s (from %s; currently %s)", mod, ver, env.caddyBin, currentVer)
			err := env.execGoGet(ctx, mod, ver, "", "")
			if err != nil {
				log.Printf("[WARNING] Could not pin %s to %s: %v", mod, ver, err)
			}
		}
	} else {
		// No reference binary, just pin Caddy core
		err = env.execGoGet(ctx, caddyModulePath, env.caddyVersion, "", "")
		if err != nil {
			return nil, err
		}
	}

	// doing an empty "go get -d" can potentially resolve some
	// ambiguities introduced by one of the plugins;
	// see https://github.com/caddyserver/xcaddy/pull/92
	// For plugin builds, we want to ensure we don't pick up incompatible assembly,
	// so we use the same build flags (especially tags) as the reference binary.
	err = env.execGoGet(ctx, "", "", "", "")
	if err != nil {
		return nil, err
	}

	log.Println("[INFO] Build environment ready")
	return env, nil
}

type environment struct {
	caddyVersion    string
	plugins         []Dependency
	caddyModulePath string
	tempFolder      string
	timeoutGoGet    time.Duration
	skipCleanup     bool
	buildFlags      string
	modFlags        string
	caddyBin        string
	pluginDir       string
}

// Close cleans up the build environment, including deleting
// the temporary folder from the disk.
func (env environment) Close() error {
	if env.skipCleanup {
		log.Printf("[INFO] Skipping cleanup as requested; leaving folder intact: %s", env.tempFolder)
		return nil
	}
	log.Printf("[INFO] Cleaning up temporary folder: %s", env.tempFolder)
	return os.RemoveAll(env.tempFolder)
}

func (env environment) newCommand(ctx context.Context, command string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = env.tempFolder
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

// newGoBuildCommand creates a new *exec.Cmd which assumes the first element in `args` is one of: build, clean, get, install, list, run, or test. The
// created command will also have the value of `XCADDY_GO_BUILD_FLAGS` appended to its arguments, if set.
func (env environment) newGoBuildCommand(ctx context.Context, goCommand string, args ...string) (*exec.Cmd, error) {
	switch goCommand {
	case "build", "clean", "get", "install", "list", "run", "test":
	default:
		return nil, fmt.Errorf("unsupported command of 'go': %s", goCommand)
	}
	cmd := env.newCommand(ctx, utils.GetGo(), goCommand)
	cmd = parseAndAppendFlags(cmd, env.buildFlags)
	cmd.Args = append(cmd.Args, args...)
	return cmd, nil
}

// newGoModCommand creates a new *exec.Cmd which assumes `args` are the args for `go mod` command. The
// created command will also have the value of `XCADDY_GO_MOD_FLAGS` appended to its arguments, if set.
func (env environment) newGoModCommand(ctx context.Context, args ...string) *exec.Cmd {
	args = append([]string{"mod"}, args...)
	return env.newCommand(ctx, utils.GetGo(), args...)
}

func parseAndAppendFlags(cmd *exec.Cmd, flags string) *exec.Cmd {
	if strings.TrimSpace(flags) == "" {
		return cmd
	}

	fs, err := shlex.Split(flags)
	if err != nil {
		log.Printf("[ERROR] Splitting arguments failed: %s", flags)
		return cmd
	}
	cmd.Args = append(cmd.Args, fs...)

	return cmd
}

func (env environment) runCommand(ctx context.Context, cmd *exec.Cmd) error {
	deadline, ok := ctx.Deadline()
	var timeout time.Duration
	// context doesn't necessarily have a deadline
	if ok {
		timeout = time.Until(deadline)
	}
	log.Printf("[INFO] exec (timeout=%s): %+v ", timeout, cmd)

	// start the command; if it fails to start, report error immediately
	err := cmd.Start()
	if err != nil {
		return err
	}

	// wait for the command in a goroutine; the reason for this is
	// very subtle: if, in our select, we do `case cmdErr := <-cmd.Wait()`,
	// then that case would be chosen immediately, because cmd.Wait() is
	// immediately available (even though it blocks for potentially a long
	// time, it can be evaluated immediately). So we have to remove that
	// evaluation from the `case` statement.
	cmdErrChan := make(chan error)
	go func() {
		cmdErrChan <- cmd.Wait()
	}()

	// unblock either when the command finishes, or when the done
	// channel is closed -- whichever comes first
	select {
	case cmdErr := <-cmdErrChan:
		// process ended; report any error immediately
		return cmdErr
	case <-ctx.Done():
		// context was canceled, either due to timeout or
		// maybe a signal from higher up canceled the parent
		// context; presumably, the OS also sent the signal
		// to the child process, so wait for it to die
		select {
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
		case <-cmdErrChan:
		}
		return ctx.Err()
	}
}

// execGoGet runs "go get -v" with the given module/version as an argument.
// Also allows passing in a second module/version pair, meant to be the main
// Caddy module/version we're building against; this will prevent the
// plugin module from causing the Caddy version to upgrade, if the plugin
// version requires a newer version of Caddy.
// See https://github.com/caddyserver/xcaddy/issues/54
func (env environment) execGoGet(ctx context.Context, modulePath, moduleVersion, caddyModulePath, caddyVersion string) error {
	mod := modulePath
	if moduleVersion != "" {
		mod += "@" + moduleVersion
	}
	caddy := caddyModulePath
	if caddyVersion != "" {
		caddy += "@" + caddyVersion
	}

	cmd, err := env.newGoBuildCommand(ctx, "get", "-v")
	if err != nil {
		return err
	}

	// using an empty string as an additional argument to "go get"
	// breaks the command since it treats the empty string as a
	// distinct argument, so we're using an if statement to avoid it.
	if caddy != "" {
		cmd.Args = append(cmd.Args, mod, caddy)
	} else {
		cmd.Args = append(cmd.Args, mod)
	}

	return env.runCommand(ctx, cmd)
}

type goModTemplateContext struct {
	CaddyModule string
	Plugins     []string
}

const mainModuleTemplate = `package main

import (
	caddycmd "{{.CaddyModule}}/cmd"

	// plug in Caddy modules here
	_ "{{.CaddyModule}}/modules/standard"
	{{- range .Plugins}}
	_ "{{.}}"
	{{- end}}
)

func main() {
	caddycmd.Main()
}
`

const pluginModuleTemplate = `package main

import (
	_ "{{.CaddyModule}}/modules/standard"
	{{- range .Plugins}}
	_ "{{.}}"
	{{- end}}
)

func main() {}
`

// originally published in: https://github.com/mholt/caddy-embed
const embeddedModuleTemplate = `package main

import (
	"embed"
	"io/fs"
	"strings"

	"{{.CaddyModule}}"
	"{{.CaddyModule}}/caddyconfig/caddyfile"
)

// embedded is what will contain your static files. The go command
// will automatically embed the files subfolder into this virtual
// file system. You can optionally change the go:embed directive
// to embed other files or folders.
//
//go:embed files
var embedded embed.FS

// files is the actual, more generic file system to be utilized.
var files fs.FS = embedded

// topFolder is the name of the top folder of the virtual
// file system. go:embed does not let us add the contents
// of a folder to the root of a virtual file system, so
// if we want to trim that root folder prefix, we need to
// also specify it in code as a string. Otherwise the
// user would need to add configuration or code to trim
// this root prefix from all filenames, e.g. specifying
// "root files" in their file_server config.
//
// It is NOT REQUIRED to change this if changing the
// go:embed directive; it is just for convenience in
// the default case.
const topFolder = "files"

func init() {
	caddy.RegisterModule(FS{})
	stripFolderPrefix()
}

// stripFolderPrefix opens the root of the file system. If it
// contains only 1 file, being a directory with the same
// name as the topFolder const, then the file system will
// be fs.Sub()'ed so the contents of the top folder can be
// accessed as if they were in the root of the file system.
// This is a convenience so most users don't have to add
// additional configuration or prefix their filenames
// unnecessarily.
func stripFolderPrefix() error {
	if f, err := files.Open("."); err == nil {
		defer f.Close()

		if dir, ok := f.(fs.ReadDirFile); ok {
			entries, err := dir.ReadDir(2)
			if err == nil &&
				len(entries) == 1 &&
				entries[0].IsDir() &&
				entries[0].Name() == topFolder {
				if sub, err := fs.Sub(embedded, topFolder); err == nil {
					files = sub
				}
			}
		}
	}
	return nil
}

// FS implements a Caddy module and fs.FS for an embedded
// file system provided by an unexported package variable.
//
// To use, simply put your files in a subfolder called
// "files", then build Caddy with your local copy of this
// plugin. Your site's files will be embedded directly
// into the binary.
//
// If the embedded file system contains only one file in
// its root which is a folder named "files", this module
// will strip that folder prefix using fs.Sub(), so that
// the contents of the folder can be accessed by name as
// if they were in the actual root of the file system.
// In other words, before: files/foo.txt, after: foo.txt.
type FS struct{}

// CaddyModule returns the Caddy module information.
func (FS) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "caddy.fs.embedded",
		New: func() caddy.Module { return new(FS) },
	}
}

func (FS) Open(name string) (fs.File, error) {
	// TODO: the file server doesn't clean up leading and trailing slashes, but embed.FS is particular so we remove them here; I wonder if the file server should be tidy in the first place
	name = strings.Trim(name, "/")
	return files.Open(name)
}

// UnmarshalCaddyfile exists so this module can be used in
// the Caddyfile, but there is nothing to unmarshal.
func (FS) UnmarshalCaddyfile(d *caddyfile.Dispenser) error { return nil }

// Interface guards
var (
	_ fs.FS                 = (*FS)(nil)
	_ caddyfile.Unmarshaler = (*FS)(nil)
)
`
