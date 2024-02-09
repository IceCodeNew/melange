// Copyright 2022 Chainguard, Inc.
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

package build

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	apko_build "chainguard.dev/apko/pkg/build"
	apko_types "chainguard.dev/apko/pkg/build/types"
	"cloud.google.com/go/storage"
	"github.com/chainguard-dev/clog"
	apkofs "github.com/chainguard-dev/go-apk/pkg/fs"
	"github.com/yookoala/realpath"
	"github.com/zealic/xignore"
	"go.opentelemetry.io/otel"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"k8s.io/kube-openapi/pkg/util/sets"

	"chainguard.dev/melange/pkg/cond"
	"chainguard.dev/melange/pkg/config"
	"chainguard.dev/melange/pkg/container"
	"chainguard.dev/melange/pkg/index"
	"chainguard.dev/melange/pkg/linter"
	"chainguard.dev/melange/pkg/sbom"
	"chainguard.dev/melange/pkg/util"
)

var ErrSkipThisArch = errors.New("error: skip this arch")

type Build struct {
	Configuration   config.Configuration
	ConfigFile      string
	SourceDateEpoch time.Time
	WorkspaceDir    string
	WorkspaceIgnore string
	// Ordered directories where to find 'uses' pipelines.
	PipelineDirs      []string
	SourceDir         string
	GuestDir          string
	SigningKey        string
	SigningPassphrase string
	Namespace         string
	GenerateIndex     bool
	EmptyWorkspace    bool
	OutDir            string
	Arch              apko_types.Architecture
	ExtraKeys         []string
	ExtraRepos        []string
	DependencyLog     string
	BinShOverlay      string
	CreateBuildLog    bool
	CacheDir          string
	ApkCacheDir       string
	CacheSource       string
	BreakpointLabel   string
	ContinueLabel     string
	foundContinuation bool
	StripOriginName   bool
	EnvFile           string
	VarsFile          string
	Runner            container.Runner
	containerConfig   *container.Config
	Debug             bool
	DebugRunner       bool
	Interactive       bool
	Remove            bool
	LogPolicy         []string
	FailOnLintWarning bool
	DefaultCPU        string
	DefaultMemory     string
	DefaultTimeout    time.Duration

	EnabledBuildOptions []string
}

func New(ctx context.Context, opts ...Option) (*Build, error) {
	b := Build{
		WorkspaceIgnore: ".melangeignore",
		SourceDir:       ".",
		OutDir:          ".",
		CacheDir:        "./melange-cache/",
		Arch:            apko_types.ParseArchitecture(runtime.GOARCH),
		LogPolicy:       []string{"builtin:stderr"},
	}

	for _, opt := range opts {
		if err := opt(&b); err != nil {
			return nil, err
		}
	}

	log := clog.New(slog.Default().Handler()).With("arch", b.Arch.ToAPK())
	ctx = clog.WithLogger(ctx, log)

	// If no workspace directory is explicitly requested, create a
	// temporary directory for it.  Otherwise, ensure we are in a
	// subdir for this specific build context.
	if b.WorkspaceDir != "" {
		// If we are continuing the build, do not modify the workspace
		// directory path.
		// TODO(kaniini): Clean up the logic for this, perhaps by signalling
		// multi-arch builds to the build context.
		if b.ContinueLabel == "" {
			b.WorkspaceDir = filepath.Join(b.WorkspaceDir, b.Arch.ToAPK())
		}

		// Get the absolute path to the workspace dir, which is needed for bind
		// mounts.
		absdir, err := filepath.Abs(b.WorkspaceDir)
		if err != nil {
			return nil, fmt.Errorf("unable to resolve path %s: %w", b.WorkspaceDir, err)
		}

		b.WorkspaceDir = absdir
	} else {
		tmpdir, err := os.MkdirTemp(b.Runner.TempDir(), "melange-workspace-*")
		if err != nil {
			return nil, fmt.Errorf("unable to create workspace dir: %w", err)
		}
		b.WorkspaceDir = tmpdir
	}

	// If no config file is explicitly requested for the build context
	// we check if .melange.yaml or melange.yaml exist.
	checks := []string{".melange.yaml", ".melange.yml", "melange.yaml", "melange.yml"}
	if b.ConfigFile == "" {
		for _, chk := range checks {
			if _, err := os.Stat(chk); err == nil {
				log.Infof("no configuration file provided -- using %s", chk)
				b.ConfigFile = chk
				break
			}
		}
	}

	// If no config file could be automatically detected, error.
	if b.ConfigFile == "" {
		return nil, fmt.Errorf("melange.yaml is missing")
	}

	parsedCfg, err := config.ParseConfiguration(ctx,
		b.ConfigFile,
		config.WithEnvFileForParsing(b.EnvFile),
		config.WithVarsFileForParsing(b.VarsFile),
		config.WithDefaultCPU(b.DefaultCPU),
		config.WithDefaultMemory(b.DefaultMemory),
		config.WithDefaultTimeout(b.DefaultTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration: %w", err)
	}

	b.Configuration = *parsedCfg

	if len(b.Configuration.Package.TargetArchitecture) == 1 &&
		b.Configuration.Package.TargetArchitecture[0] == "all" {
		log.Warnf("target-architecture: ['all'] is deprecated and will become an error; remove this field to build for all available archs")
	} else if len(b.Configuration.Package.TargetArchitecture) != 0 &&
		!sets.NewString(b.Configuration.Package.TargetArchitecture...).Has(b.Arch.ToAPK()) {
		return nil, ErrSkipThisArch
	}

	// SOURCE_DATE_EPOCH will always overwrite the build flag
	if _, ok := os.LookupEnv("SOURCE_DATE_EPOCH"); ok {
		t, err := util.SourceDateEpoch(b.SourceDateEpoch)
		if err != nil {
			return nil, err
		}
		b.SourceDateEpoch = t
	}

	// Check that we actually can run things in containers.
	if !b.Runner.TestUsability(ctx) {
		return nil, fmt.Errorf("unable to run containers using %s, specify --runner and one of %s", b.Runner.Name(), GetAllRunners())
	}

	// Apply build options to the context.
	for _, optName := range b.EnabledBuildOptions {
		log.Infof("applying configuration patches for build option %s", optName)

		if opt, ok := b.Configuration.Options[optName]; ok {
			if err := b.ApplyBuildOption(opt); err != nil {
				return nil, err
			}
		}
	}

	return &b, nil
}

func (b *Build) Close(ctx context.Context) error {
	errs := []error{}
	if b.Remove {
		clog.FromContext(ctx).Infof("deleting guest dir %s", b.GuestDir)
		errs = append(errs, os.RemoveAll(b.GuestDir))
		errs = append(errs, os.RemoveAll(b.WorkspaceDir))
		if b.containerConfig != nil && b.containerConfig.ImgRef != "" {
			errs = append(errs, b.Runner.OCIImageLoader().RemoveImage(ctx, b.containerConfig.ImgRef))
		}
	}
	errs = append(errs, b.Runner.Close())

	return errors.Join(errs...)
}

// BuildGuest invokes apko to build the guest environment, returning a reference to the image
// loaded by the OCI Image loader.
func (b *Build) BuildGuest(ctx context.Context, imgConfig apko_types.ImageConfiguration, guestFS apkofs.FullFS) (string, error) {
	log := clog.FromContext(ctx)
	ctx, span := otel.Tracer("melange").Start(ctx, "BuildGuest")
	defer span.End()

	bc, err := apko_build.New(ctx, guestFS,
		apko_build.WithImageConfiguration(imgConfig),
		apko_build.WithArch(b.Arch),
		apko_build.WithExtraKeys(b.ExtraKeys),
		apko_build.WithExtraRepos(b.ExtraRepos),
		apko_build.WithCacheDir(b.ApkCacheDir, false), // TODO: Replace with real offline plumbing
	)
	if err != nil {
		return "", fmt.Errorf("unable to create build context: %w", err)
	}

	bc.Summarize(ctx)

	// lay out the contents for the image in a directory.
	if err := bc.BuildImage(ctx); err != nil {
		return "", fmt.Errorf("unable to generate image: %w", err)
	}
	// if the runner needs an image, create an OCI image from the directory and load it.
	loader := b.Runner.OCIImageLoader()
	if loader == nil {
		return "", fmt.Errorf("runner %s does not support OCI image loading", b.Runner.Name())
	}
	layerTarGZ, layer, err := bc.ImageLayoutToLayer(ctx)
	if err != nil {
		return "", err
	}
	defer os.Remove(layerTarGZ)

	log.Infof("using %s for image layer", layerTarGZ)

	ref, err := loader.LoadImage(ctx, layer, b.Arch, bc)
	if err != nil {
		return "", err
	}

	log.Debugf("pushed %s as %v", layerTarGZ, ref)
	log.Debug("successfully built workspace with apko")
	return ref, nil
}

func copyFile(base, src, dest string, perm fs.FileMode) error {
	basePath := filepath.Join(base, src)
	destPath := filepath.Join(dest, src)
	destDir := filepath.Dir(destPath)

	inF, err := os.Open(basePath)
	if err != nil {
		return err
	}
	defer inF.Close()

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("mkdir -p %s: %w", destDir, err)
	}

	outF, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", destPath, err)
	}
	defer outF.Close()

	if _, err := io.Copy(outF, inF); err != nil {
		return err
	}

	if err := os.Chmod(destPath, perm); err != nil {
		return err
	}

	return nil
}

// ApplyBuildOption applies a patch described by a BuildOption to a package build.
func (b *Build) ApplyBuildOption(bo config.BuildOption) error {
	// Patch the variables block.
	if b.Configuration.Vars == nil {
		b.Configuration.Vars = make(map[string]string)
	}

	for k, v := range bo.Vars {
		b.Configuration.Vars[k] = v
	}

	// Patch the build environment configuration.
	lo := bo.Environment.Contents.Packages
	b.Configuration.Environment.Contents.Packages = append(b.Configuration.Environment.Contents.Packages, lo.Add...)

	for _, pkg := range lo.Remove {
		pkgList := b.Configuration.Environment.Contents.Packages

		for pos, ppkg := range pkgList {
			if pkg == ppkg {
				pkgList[pos] = pkgList[len(pkgList)-1]
				pkgList = pkgList[:len(pkgList)-1]
			}
		}

		b.Configuration.Environment.Contents.Packages = pkgList
	}

	return nil
}

func (b *Build) loadIgnoreRules(ctx context.Context) ([]*xignore.Pattern, error) {
	log := clog.FromContext(ctx)
	ignorePath := filepath.Join(b.SourceDir, b.WorkspaceIgnore)

	ignorePatterns := []*xignore.Pattern{}

	if _, err := os.Stat(ignorePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ignorePatterns, nil
		}

		return nil, err
	}

	log.Infof("loading ignore rules from %s", ignorePath)

	inF, err := os.Open(ignorePath)
	if err != nil {
		return nil, err
	}
	defer inF.Close()

	ignF := xignore.Ignorefile{}
	if err := ignF.FromReader(inF); err != nil {
		return nil, err
	}

	for _, rule := range ignF.Patterns {
		pattern := xignore.NewPattern(rule)

		if err := pattern.Prepare(); err != nil {
			return nil, err
		}

		ignorePatterns = append(ignorePatterns, pattern)
	}

	return ignorePatterns, nil
}

func (b *Build) OverlayBinSh() error {
	if b.BinShOverlay == "" {
		return nil
	}

	targetPath := filepath.Join(b.GuestDir, "bin", "sh")

	inF, err := os.Open(b.BinShOverlay)
	if err != nil {
		return fmt.Errorf("copying overlay /bin/sh: %w", err)
	}
	defer inF.Close()

	// We unlink the target first because it might be a symlink.
	if err := os.Remove(targetPath); err != nil {
		return fmt.Errorf("copying overlay /bin/sh: %w", err)
	}

	outF, err := os.Create(targetPath)
	if err != nil {
		return fmt.Errorf("copying overlay /bin/sh: %w", err)
	}
	defer outF.Close()

	if _, err := io.Copy(outF, inF); err != nil {
		return fmt.Errorf("copying overlay /bin/sh: %w", err)
	}

	if err := os.Chmod(targetPath, 0o755); err != nil {
		return fmt.Errorf("setting overlay /bin/sh executable: %w", err)
	}

	return nil
}

func fetchBucket(ctx context.Context, cacheSource string, cmm CacheMembershipMap) (string, error) {
	log := clog.FromContext(ctx)
	tmp, err := os.MkdirTemp("", "melange-cache")
	if err != nil {
		return "", err
	}
	bucket, prefix, _ := strings.Cut(strings.TrimPrefix(cacheSource, "gs://"), "/")

	client, err := storage.NewClient(ctx)
	if err != nil {
		log.Infof("downgrading to anonymous mode: %s", err)

		client, err = storage.NewClient(ctx, option.WithoutAuthentication())
		if err != nil {
			return "", fmt.Errorf("failed to get storage client: %w", err)
		}
	}

	bh := client.Bucket(bucket)
	it := bh.Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		} else if err != nil {
			return tmp, fmt.Errorf("failed to get next remote cache object: %w", err)
		}
		on := attrs.Name
		if !cmm[on] {
			continue
		}
		rc, err := bh.Object(on).NewReader(ctx)
		if err != nil {
			return tmp, fmt.Errorf("failed to get reader for next remote cache object %s: %w", on, err)
		}
		w, err := os.Create(filepath.Join(tmp, on))
		if err != nil {
			return tmp, err
		}
		if _, err := io.Copy(w, rc); err != nil {
			return tmp, fmt.Errorf("failed to copy remote cache object %s: %w", on, err)
		}
		if err := rc.Close(); err != nil {
			return tmp, fmt.Errorf("failed to close remote cache object %s: %w", on, err)
		}
		log.Infof("cached gs://%s/%s -> %s", bucket, on, w.Name())
	}

	return tmp, nil
}

// IsBuildLess returns true if the build context does not actually do any building.
// TODO(kaniini): Improve the heuristic for this by checking for uses/runs statements
// in the pipeline.
func (b *Build) IsBuildLess() bool {
	return len(b.Configuration.Pipeline) == 0
}

func (b *Build) PopulateCache(ctx context.Context) error {
	log := clog.FromContext(ctx)
	ctx, span := otel.Tracer("melange").Start(ctx, "PopulateCache")
	defer span.End()

	if b.CacheDir == "" {
		return nil
	}

	cmm, err := cacheItemsForBuild(b.ConfigFile)
	if err != nil {
		return fmt.Errorf("while determining which objects to fetch: %w", err)
	}

	if b.CacheSource != "" {
		log.Debugf("populating cache from %s", b.CacheSource)
	}

	// --cache-dir=gs://bucket/path/to/cache first pulls all found objects to a
	// tmp dir which is subsequently used as the cache.
	if strings.HasPrefix(b.CacheSource, "gs://") {
		tmp, err := fetchBucket(ctx, b.CacheSource, cmm)
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		log.Infof("cache bucket copied to %s", tmp)

		fsys := os.DirFS(tmp)

		// mkdir /var/cache/melange
		if err := os.MkdirAll(b.CacheDir, 0o755); err != nil {
			return err
		}

		// --cache-dir doesn't exist, nothing to do.
		if _, err := fs.Stat(fsys, "."); errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			fi, err := d.Info()
			if err != nil {
				return err
			}

			mode := fi.Mode()
			if !mode.IsRegular() {
				return nil
			}

			// Skip files in the cache that aren't named like sha256:... or sha512:...
			// This is likely a bug, and won't be matched by any fetch.
			base := filepath.Base(fi.Name())
			if !strings.HasPrefix(base, "sha256:") &&
				!strings.HasPrefix(base, "sha512:") {
				return nil
			}

			log.Debugf("  -> %s", path)

			if err := copyFile(tmp, path, b.CacheDir, mode.Perm()); err != nil {
				return err
			}

			return nil
		})
	}

	return nil
}

func (b *Build) PopulateWorkspace(ctx context.Context, src fs.FS) error {
	log := clog.FromContext(ctx)
	_, span := otel.Tracer("melange").Start(ctx, "PopulateWorkspace")
	defer span.End()

	ignorePatterns, err := b.loadIgnoreRules(ctx)
	if err != nil {
		return err
	}

	return fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		mode := fi.Mode()
		if !mode.IsRegular() {
			return nil
		}

		for _, pat := range ignorePatterns {
			if pat.Match(path) {
				return nil
			}
		}

		log.Debugf("  -> %s", path)

		if err := copyFile(b.SourceDir, path, b.WorkspaceDir, mode.Perm()); err != nil {
			return err
		}

		return nil
	})
}

func (pb *PipelineBuild) ShouldRun(sp config.Subpackage) (bool, error) {
	if sp.If == "" {
		return true, nil
	}

	lookupWith := func(key string) (string, error) {
		mutated, err := MutateWith(pb, map[string]string{})
		if err != nil {
			return "", err
		}
		nk := fmt.Sprintf("${{%s}}", key)
		return mutated[nk], nil
	}

	result, err := cond.Evaluate(sp.If, lookupWith)
	if err != nil {
		return false, fmt.Errorf("evaluating subpackage if-conditional: %w", err)
	}

	return result, nil
}

type linterTarget struct {
	pkgName string
	checks  config.Checks
}

func (b *Build) BuildPackage(ctx context.Context) error {
	log := clog.FromContext(ctx)
	ctx, span := otel.Tracer("melange").Start(ctx, "BuildPackage")
	defer span.End()

	b.Summarize(ctx)

	if to := b.Configuration.Package.Timeout; to > 0 {
		tctx, cancel := context.WithTimeoutCause(ctx, to,
			fmt.Errorf("build exceeded its timeout of %s", to))
		defer cancel()
		ctx = tctx
	}

	pkg := &b.Configuration.Package

	pb := PipelineBuild{
		Build:   b,
		Package: pkg,
	}

	if b.GuestDir == "" {
		guestDir, err := os.MkdirTemp(b.Runner.TempDir(), "melange-guest-*")
		if err != nil {
			return fmt.Errorf("unable to make guest directory: %w", err)
		}
		b.GuestDir = guestDir
	}

	log.Infof("evaluating pipelines for package requirements")
	for _, p := range b.Configuration.Pipeline {
		// fine to pass nil for config, since not running in container.
		pctx := NewPipelineContext(&p, &b.Configuration.Environment, nil, b.PipelineDirs)

		if err := pctx.ApplyNeeds(ctx, &pb); err != nil {
			return fmt.Errorf("unable to apply pipeline requirements: %w", err)
		}
	}

	for _, spkg := range b.Configuration.Subpackages {
		spkg := spkg
		pb.Subpackage = &spkg
		for _, p := range spkg.Pipeline {
			// fine to pass nil for config, since not running in container.
			pctx := NewPipelineContext(&p, &b.Configuration.Environment, nil, b.PipelineDirs)
			if err := pctx.ApplyNeeds(ctx, &pb); err != nil {
				return fmt.Errorf("unable to apply pipeline requirements: %w", err)
			}
		}
	}
	pb.Subpackage = nil

	if b.EmptyWorkspace {
		log.Infof("empty workspace requested")
	} else {
		// Prepare workspace directory
		if err := os.MkdirAll(b.WorkspaceDir, 0755); err != nil {
			return fmt.Errorf("mkdir -p %s: %w", b.WorkspaceDir, err)
		}

		log.Infof("populating workspace %s from %s", b.WorkspaceDir, b.SourceDir)
		if err := b.PopulateWorkspace(ctx, os.DirFS(b.SourceDir)); err != nil {
			return fmt.Errorf("unable to populate workspace: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Join(b.WorkspaceDir, "melange-out", b.Configuration.Package.Name), 0o755); err != nil {
		return err
	}

	linterQueue := []linterTarget{}
	cfg := b.WorkspaceConfig(ctx)

	if !b.IsBuildLess() {
		// Prepare guest directory
		if err := os.MkdirAll(b.GuestDir, 0755); err != nil {
			return fmt.Errorf("mkdir -p %s: %w", b.GuestDir, err)
		}

		log.Infof("building workspace in '%s' with apko", b.GuestDir)

		guestFS := apkofs.DirFS(b.GuestDir, apkofs.WithCreateDir())
		imgRef, err := b.BuildGuest(ctx, b.Configuration.Environment, guestFS)
		if err != nil {
			return fmt.Errorf("unable to build guest: %w", err)
		}

		cfg.ImgRef = imgRef
		log.Infof("ImgRef = %s", cfg.ImgRef)

		// TODO(kaniini): Make overlay-binsh work with Docker and Kubernetes.
		// Probably needs help from apko.
		if err := b.OverlayBinSh(); err != nil {
			return fmt.Errorf("unable to install overlay /bin/sh: %w", err)
		}

		if err := b.PopulateCache(ctx); err != nil {
			return fmt.Errorf("unable to populate cache: %w", err)
		}

		if err := b.Runner.StartPod(ctx, cfg); err != nil {
			return fmt.Errorf("unable to start pod: %w", err)
		}
		if !b.DebugRunner {
			defer func() {
				if err := b.Runner.TerminatePod(context.WithoutCancel(ctx), cfg); err != nil {
					log.Warnf("unable to terminate pod: %s", err)
				}
			}()
		}

		// run the main pipeline
		log.Debug("running the main pipeline")
		for _, p := range b.Configuration.Pipeline {
			pctx := NewPipelineContext(&p, &b.Configuration.Environment, cfg, b.PipelineDirs)
			if _, err := pctx.Run(ctx, &pb); err != nil {
				return fmt.Errorf("unable to run pipeline: %w", err)
			}
		}

		// add the main package to the linter queue
		lintTarget := linterTarget{
			pkgName: b.Configuration.Package.Name,
			checks:  b.Configuration.Package.Checks,
		}
		linterQueue = append(linterQueue, lintTarget)
	}

	namespace := b.Namespace
	if namespace == "" {
		namespace = "unknown"
	}

	// run any pipelines for subpackages
	for _, sp := range b.Configuration.Subpackages {
		sp := sp
		if !b.IsBuildLess() {
			log.Infof("running pipeline for subpackage %s", sp.Name)
			pb.Subpackage = &sp

			result, err := pb.ShouldRun(sp)
			if err != nil {
				return err
			}
			if !result {
				continue
			}

			for _, p := range sp.Pipeline {
				pctx := NewPipelineContext(&p, &b.Configuration.Environment, cfg, b.PipelineDirs)
				if _, err := pctx.Run(ctx, &pb); err != nil {
					return fmt.Errorf("unable to run pipeline: %w", err)
				}
			}
		}

		if err := os.MkdirAll(filepath.Join(b.WorkspaceDir, "melange-out", sp.Name), 0o755); err != nil {
			return err
		}

		// add the main package to the linter queue
		lintTarget := linterTarget{
			pkgName: sp.Name,
			checks:  sp.Checks,
		}
		linterQueue = append(linterQueue, lintTarget)
	}

	// Retrieve the post build workspace from the runner
	log.Infof("retrieving workspace from builder: %s", cfg.PodID)
	fs := apkofs.DirFS(b.WorkspaceDir)
	if err := b.RetrieveWorkspace(ctx, fs); err != nil {
		return fmt.Errorf("retrieving workspace: %w", err)
	}
	log.Infof("retrieved and wrote post-build workspace to: %s", b.WorkspaceDir)

	// perform package linting
	for _, lt := range linterQueue {
		log.Infof("running package linters for %s", lt.pkgName)

		path := filepath.Join(b.WorkspaceDir, "melange-out", lt.pkgName)
		linters := lt.checks.GetLinters()

		var innerErr error
		if err := linter.LintBuild(lt.pkgName, path, func(err error) {
			if b.FailOnLintWarning {
				innerErr = err
			} else {
				log.Warnf("WARNING: %v", err)
			}
		}, linters); err != nil {
			return fmt.Errorf("package linter error: %w", err)
		} else if innerErr != nil {
			return fmt.Errorf("package linter warning: %w", err)
		}
	}

	// Run the SBOM generator.
	generator := sbom.NewGenerator()

	// generate SBOMs for subpackages
	for _, sp := range b.Configuration.Subpackages {
		sp := sp

		if !b.IsBuildLess() {
			log.Infof("generating SBOM for subpackage %s", sp.Name)
			pb.Subpackage = &sp

			result, err := pb.ShouldRun(sp)
			if err != nil {
				return err
			}
			if !result {
				continue
			}
		}

		if err := generator.GenerateSBOM(ctx, &sbom.Spec{
			Path:           filepath.Join(b.WorkspaceDir, "melange-out", sp.Name),
			PackageName:    sp.Name,
			PackageVersion: fmt.Sprintf("%s-r%d", b.Configuration.Package.Version, b.Configuration.Package.Epoch),
			License:        b.Configuration.Package.LicenseExpression(),
			Copyright:      b.Configuration.Package.FullCopyright(),
			Namespace:      namespace,
			Arch:           b.Arch.ToAPK(),
		}); err != nil {
			return fmt.Errorf("writing SBOMs: %w", err)
		}
	}

	if err := generator.GenerateSBOM(ctx, &sbom.Spec{
		Path:           filepath.Join(b.WorkspaceDir, "melange-out", b.Configuration.Package.Name),
		PackageName:    b.Configuration.Package.Name,
		PackageVersion: fmt.Sprintf("%s-r%d", b.Configuration.Package.Version, b.Configuration.Package.Epoch),
		License:        b.Configuration.Package.LicenseExpression(),
		Copyright:      b.Configuration.Package.FullCopyright(),
		Namespace:      namespace,
		Arch:           b.Arch.ToAPK(),
	}); err != nil {
		return fmt.Errorf("writing SBOMs: %w", err)
	}

	// emit main package
	if err := pb.Emit(ctx, pkg); err != nil {
		return fmt.Errorf("unable to emit package: %w", err)
	}

	// emit subpackages
	for _, sp := range b.Configuration.Subpackages {
		sp := sp
		pb.Subpackage = &sp

		result, err := pb.ShouldRun(sp)
		if err != nil {
			return err
		}
		if !result {
			continue
		}

		if err := pb.Emit(ctx, pkgFromSub(&sp)); err != nil {
			return fmt.Errorf("unable to emit package: %w", err)
		}
	}

	if !b.IsBuildLess() {
		// clean build guest container
		if err := os.RemoveAll(b.GuestDir); err != nil {
			log.Infof("WARNING: unable to clean guest container: %s", err)
		}
	}

	// clean build environment
	// TODO(epsilon-phase): implement a way to clean up files that are not owned by the user
	// that is running melange. files created inside the build not owned by the build user are
	// not be possible to delete with this strategy.
	if err := os.RemoveAll(b.WorkspaceDir); err != nil {
		log.Infof("WARNING: unable to clean workspace: %s", err)
	}

	// generate APKINDEX.tar.gz and sign it
	if b.GenerateIndex {
		packageDir := filepath.Join(pb.Build.OutDir, pb.Build.Arch.ToAPK())
		log.Infof("generating apk index from packages in %s", packageDir)

		var apkFiles []string
		pkgFileName := fmt.Sprintf("%s-%s-r%d.apk", b.Configuration.Package.Name, b.Configuration.Package.Version, b.Configuration.Package.Epoch)
		apkFiles = append(apkFiles, filepath.Join(packageDir, pkgFileName))

		for _, subpkg := range b.Configuration.Subpackages {
			subpkg := subpkg
			pb.Subpackage = &subpkg

			result, err := pb.ShouldRun(subpkg)
			if err != nil {
				return err
			}
			if !result {
				continue
			}

			subpkgFileName := fmt.Sprintf("%s-%s-r%d.apk", subpkg.Name, b.Configuration.Package.Version, b.Configuration.Package.Epoch)
			apkFiles = append(apkFiles, filepath.Join(packageDir, subpkgFileName))
		}

		opts := []index.Option{
			index.WithPackageFiles(apkFiles),
			index.WithSigningKey(b.SigningKey),
			index.WithMergeIndexFileFlag(true),
			index.WithIndexFile(filepath.Join(packageDir, "APKINDEX.tar.gz")),
		}

		idx, err := index.New(opts...)
		if err != nil {
			return fmt.Errorf("unable to create index: %w", err)
		}

		if err := idx.GenerateIndex(ctx); err != nil {
			return fmt.Errorf("unable to generate index: %w", err)
		}

		if err := idx.WriteJSONIndex(filepath.Join(packageDir, "APKINDEX.json")); err != nil {
			return fmt.Errorf("unable to generate JSON index: %w", err)
		}
	}

	return nil
}

func (b *Build) SummarizePaths(ctx context.Context) {
	log := clog.FromContext(ctx)
	log.Infof("  workspace dir: %s", b.WorkspaceDir)

	if b.GuestDir != "" {
		log.Infof("  guest dir: %s", b.GuestDir)
	}
}

func (b *Build) Summarize(ctx context.Context) {
	log := clog.FromContext(ctx)
	log.Infof("melange is building:")
	log.Infof("  configuration file: %s", b.ConfigFile)
	b.SummarizePaths(ctx)
}

// BuildFlavor determines if a build context uses glibc or musl, it returns
// "gnu" for GNU systems, and "musl" for musl systems.
func (b *Build) BuildFlavor() string {
	for _, dir := range []string{"lib", "lib64"} {
		if _, err := os.Stat(filepath.Join(b.GuestDir, dir, "libc.so.6")); err == nil {
			return "gnu"
		}
	}

	return "musl"
}

// BuildTripletGnu returns the GNU autoconf build triplet, for example
// `x86_64-pc-linux-gnu`.
func (b *Build) BuildTripletGnu() string {
	return b.Arch.ToTriplet(b.BuildFlavor())
}

// BuildTripletRust returns the Rust/Cargo build triplet, for example
// `x86_64-unknown-linux-gnu`.
func (b *Build) BuildTripletRust() string {
	return b.Arch.ToRustTriplet(b.BuildFlavor())
}

func (b *Build) buildWorkspaceConfig(ctx context.Context) *container.Config {
	log := clog.FromContext(ctx)
	if b.IsBuildLess() {
		return &container.Config{
			Arch: b.Arch,
		}
	}

	mounts := []container.BindMount{
		{Source: b.WorkspaceDir, Destination: container.DefaultWorkspaceDir},
		{Source: "/etc/resolv.conf", Destination: container.DefaultResolvConfPath},
	}

	if b.CacheDir != "" {
		if fi, err := os.Stat(b.CacheDir); err == nil && fi.IsDir() {
			mountSource, err := realpath.Realpath(b.CacheDir)
			if err != nil {
				log.Infof("could not resolve path for --cache-dir: %s", err)
			}

			mounts = append(mounts, container.BindMount{Source: mountSource, Destination: container.DefaultCacheDir})
		} else {
			log.Infof("--cache-dir %s not a dir; skipping", b.CacheDir)
		}
	}

	// TODO(kaniini): Disable networking capability according to the pipeline requirements.
	caps := container.Capabilities{
		Networking: true,
	}

	cfg := container.Config{
		Arch:         b.Arch,
		PackageName:  b.Configuration.Package.Name,
		Mounts:       mounts,
		Capabilities: caps,
		Environment: map[string]string{
			"SOURCE_DATE_EPOCH": fmt.Sprintf("%d", b.SourceDateEpoch.Unix()),
		},
		Timeout: b.Configuration.Package.Timeout,
	}

	if b.Configuration.Package.Resources != nil {
		cfg.CPU = b.Configuration.Package.Resources.CPU
		cfg.Memory = b.Configuration.Package.Resources.Memory
	}

	for k, v := range b.Configuration.Environment.Environment {
		cfg.Environment[k] = v
	}

	return &cfg
}

func (b *Build) WorkspaceConfig(ctx context.Context) *container.Config {
	if b.containerConfig == nil {
		b.containerConfig = b.buildWorkspaceConfig(ctx)
	}

	return b.containerConfig
}

// RetrieveWorkspace retrieves the workspace from the container and unpacks it
// to the workspace directory. The workspace retrieved from the runner is in a
// tar stream containing the workspace contents rooted at ./melange-out
func (b *Build) RetrieveWorkspace(ctx context.Context, fs apkofs.FullFS) error {
	ctx, span := otel.Tracer("melange").Start(ctx, "RetrieveWorkspace")
	defer span.End()

	r, err := b.Runner.WorkspaceTar(ctx, b.containerConfig)
	if err != nil {
		return err
	} else if r == nil {
		return nil
	}
	defer r.Close()

	gr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gr.Close()
	tr := tar.NewReader(gr)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if fi, err := fs.Stat(hdr.Name); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				if target, err := fs.Readlink(hdr.Name); err == nil {
					if fi, err = fs.Stat(target); err == nil && fi.IsDir() {
						break
					}
				}
			}

			if err := fs.MkdirAll(hdr.Name, hdr.FileInfo().Mode().Perm()); err != nil {
				return fmt.Errorf("unable to create directory %s: %w", hdr.Name, err)
			}

		case tar.TypeReg:
			f, err := fs.OpenFile(hdr.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, hdr.FileInfo().Mode())
			if err != nil {
				return fmt.Errorf("unable to open file %s: %w", hdr.Name, err)
			}

			if _, err := io.CopyN(f, tr, hdr.Size); err != nil {
				return fmt.Errorf("unable to copy file %s: %w", hdr.Name, err)
			}

			if err := f.Close(); err != nil {
				return fmt.Errorf("unable to close file %s: %w", hdr.Name, err)
			}

		case tar.TypeSymlink:
			if target, err := fs.Readlink(hdr.Name); err == nil && target == hdr.Linkname {
				continue
			}

			if err := fs.Symlink(hdr.Linkname, hdr.Name); err != nil {
				return fmt.Errorf("unable to create symlink %s -> %s: %w", hdr.Name, hdr.Linkname, err)
			}

		case tar.TypeLink:
			if err := fs.Link(hdr.Linkname, hdr.Name); err != nil {
				return err
			}

		default:
			return fmt.Errorf("unexpected tar type %d for %s", hdr.Typeflag, hdr.Name)
		}

		for k, v := range hdr.PAXRecords {
			if !strings.HasPrefix(k, "SCHILY.xattr.") {
				continue
			}
			attrName := strings.TrimPrefix(k, "SCHILY.xattr.")
			fmt.Println("setting xattr", attrName, "on", hdr.Name)
			if err := fs.SetXattr(hdr.Name, attrName, []byte(v)); err != nil {
				return fmt.Errorf("unable to set xattr %s on %s: %w", attrName, hdr.Name, err)
			}
		}
	}

	return nil
}
