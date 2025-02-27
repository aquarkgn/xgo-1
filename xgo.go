// Wrapper around the GCO cross compiler docker container.
package main

import (
	"flag"
	"fmt"
	"go/build"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

var version = "dev"
var depsCache = filepath.Join(os.TempDir(), "xgo-cache")

// Cross compilation docker containers
var dockerDist = "ghcr.io/crazy-max/xgo"

// Command line arguments to fine tune the compilation
var (
	// Go版本
	goVersion = flag.String("go-version", "latest", "Go version (default: latest)")
	// Go代理地址
	goProxy = flag.String("go-proxy", "", "Go模块设置全局代理")
	// git 子模块，未验证参数是否可用
	srcPackage = flag.String("pkg", "", "git 子模块，未验证参数是否可用:Sub-package to build if not root import")
	// 项目Git远程仓库
	srcRemote = flag.String("remote", "", "项目Git远程仓库")
	// 项目Git分支
	srcBranch = flag.String("branch", "", "项目Git分支")

	crossDeps = flag.String("deps", "", "CGO dependencies (configure/make based archives)")
	crossArgs = flag.String("depsargs", "", "CGO dependency configure arguments")
	// 交叉编译目标
	targets     = flag.String("targets", "*/*", "要构建的目标 os/arch 的逗号分隔列表: */* or linux/amd64,darwin/amd64")
	dockerRepo  = flag.String("docker-repo", "", "使用自定义docker repo而不是官方分发")
	dockerImage = flag.String("docker-image", "", "使用自定义docker图像而不是官方分发")
	// 项目根目录
	projectPath = flag.String("project-path", "", "项目根目录")
	// 项目命令所在相对目录，为空时默认为项目根目录 例如：cmd/xxx
	cmdPath = flag.String("cmd-path", ".", "项目命令所在相对目录，为空时默认为项目根目录 例如：cmd/xxx")
	// Go构建命令目录
	binPath = flag.String("bin-path", "bin", "Go构建命令目录")
	// Go构建命令前缀
	commandPrefix = flag.String("command-prefix", "", "Go构建命令前缀")
)

// ConfigFlags is a simple set of flags to define the environment and dependencies.
type ConfigFlags struct {
	Package      string   // Sub-package to build if not root import
	Prefix       string   // Prefix to use for output naming
	Remote       string   // Version control remote repository to build
	Branch       string   // Version control branch to build
	Dependencies string   // CGO dependencies (configure/make based archives)
	Arguments    string   // CGO dependency configure arguments
	Targets      []string // 项目命令所在相对目录，为空时默认为项目根目录 例如：cmd/xxx
	ProjectPath  string   // 项目根目录
	BinPath      string   // Go构建命令目录
	CmdPath      string   // 项目命令所在相对目录，为空时默认为项目根目录 例如：cmd/xxx
}

// Command line arguments to pass to go build
var (
	// Go编译时打印包的名称
	buildVerbose = flag.Bool("v", false, "Go编译时打印包的名称")
	// 命令在执行生成时打印命令
	buildSteps = flag.Bool("x", false, "命令在执行生成时打印命令")
	// 启用数据竞争检测（仅在amd64上支持）
	buildRace = flag.Bool("race", false, "启用数据竞争检测（仅在amd64上支持）")

	buildTags     = flag.String("tags", "", "List of build tags to consider satisfied during the build")
	buildLdFlags  = flag.String("build-ldflags", "", "每次go工具链接调用时传递的参数")
	buildMode     = flag.String("build-mode", "default", "Indicates which kind of object file to build(default|archive|exe|pie)")
	buildVCS      = flag.String("build-vcs", "", "Whether to stamp binaries with version control information (none|git|hg|svn|bzr)")
	buildTrimPath = flag.Bool("build-trim-path", false, "从生成的可执行文件中删除所有文件系统路径")
)

// BuildFlags is a simple collection of flags to fine tune a build.
type BuildFlags struct {
	Verbose  bool   // Print the names of packages as they are compiled
	Steps    bool   // Print the command as executing the builds
	Race     bool   // Enable data race detection (supported only on amd64)
	Tags     string // List of build tags to consider satisfied during the build
	LdFlags  string // Arguments to pass on each go tool link invocation
	Mode     string // Indicates which kind of object file to build
	VCS      string // Whether to stamp binaries with version control information
	TrimPath bool   // Remove all file system paths from the resulting executable
}

func main() {
	log.SetFlags(0)
	defer log.Println("INFO: Completed!")
	log.Printf("INFO: Starting xgo/%s", version)

	// Retrieve the CLI flags and the execution environment
	flag.Parse()

	if *projectPath == "" {
		*projectPath, _ = filepath.Abs("")
	}

	// 组装交叉编译环境和构建选项
	config := &ConfigFlags{
		Package:      *srcPackage,
		Remote:       *srcRemote,
		Branch:       *srcBranch,
		Prefix:       *commandPrefix,
		Dependencies: *crossDeps,
		Arguments:    *crossArgs,
		Targets:      strings.Split(*targets, ","),
		ProjectPath:  *projectPath,
		BinPath:      filepath.Join(*projectPath, *binPath),
		CmdPath:      filepath.Join(*projectPath, *cmdPath),
	}
	log.Printf("DBG: config: %+v", config)
	flags := &BuildFlags{
		Verbose:  *buildVerbose,
		Steps:    *buildSteps,
		Race:     *buildRace,
		Tags:     *buildTags,
		LdFlags:  *buildLdFlags,
		Mode:     *buildMode,
		VCS:      *buildVCS,
		TrimPath: *buildTrimPath,
	}
	log.Printf("DBG: flags: %+v", flags)

	xgoInXgo := os.Getenv("XGO_IN_XGO") == "1"
	if xgoInXgo {
		depsCache = "/deps-cache"
	}
	// Only use docker images if we're not already inside out own image
	image := ""

	if !xgoInXgo {
		// Ensure docker is available
		if err := checkDocker(); err != nil {
			log.Fatalf("ERROR: Failed to check docker installation: %v.", err)
		}
		// Select the image to use, either official or custom
		image = fmt.Sprintf("%s:%s", dockerDist, *goVersion)
		if *dockerImage != "" {
			image = *dockerImage
		} else if *dockerRepo != "" {
			image = fmt.Sprintf("%s:%s", *dockerRepo, *goVersion)
		}
		// Check that all required images are available
		found := checkDockerImage(image)
		switch {
		case !found:
			fmt.Println("not found!")
			if err := pullDockerImage(image); err != nil {
				log.Fatalf("ERROR: Failed to pull docker image from the registry: %v.", err)
			}
		default:
			log.Println("INFO: Docker image found!")
		}
	}
	// Cache all external dependencies to prevent always hitting the internet
	if *crossDeps != "" {
		if err := os.MkdirAll(depsCache, 0751); err != nil {
			log.Fatalf("ERROR: Failed to create dependency cache: %v.", err)
		}
		// Download all missing dependencies
		for _, dep := range strings.Split(*crossDeps, " ") {
			if url := strings.TrimSpace(dep); len(url) > 0 {
				path := filepath.Join(depsCache, filepath.Base(url))

				if _, err := os.Stat(path); err != nil {
					log.Printf("INFO: Downloading new dependency: %s...", url)
					out, err := os.Create(path)
					if err != nil {
						log.Fatalf("ERROR: Failed to create dependency file: %v", err)
					}
					res, err := http.Get(url)
					if err != nil {
						log.Fatalf("ERROR: Failed to retrieve dependency: %v", err)
					}
					defer res.Body.Close()

					if _, err := io.Copy(out, res.Body); err != nil {
						log.Fatalf("INFO: Failed to download dependency: %v", err)
					}
					out.Close()

					log.Printf("INFO: New dependency cached: %s.", path)
				} else {
					fmt.Printf("INFO: Dependency already cached: %s.", path)
				}
			}
		}
	}

	var err error
	if config.BinPath != "" {
		config.BinPath, err = filepath.Abs(*binPath)
		if err != nil {
			log.Fatalf("ERROR: Failed to resolve destination path (%s): %v.", *binPath, err)
		}
	}

	// 在容器或当前系统中执行交叉编译
	if !xgoInXgo {
		err = compile(image, config, flags)
	} else {
		err = compileContained(config, flags)
	}
	if err != nil {
		log.Fatalf("ERROR: Failed to cross compile package: %v.", err)
	}
}

// Checks whether a docker installation can be found and is functional.
// 检查是否可以找到docker安装并且功能正常。
func checkDocker() error {
	log.Println("INFO: Checking docker installation...")
	if err := run(exec.Command("docker", "version")); err != nil {
		return err
	}
	fmt.Println()
	return nil
}

// Checks whether a required docker image is available locally.
func checkDockerImage(image string) bool {
	log.Printf("INFO: Checking for required docker image %s... ", image)
	err := exec.Command("docker", "image", "inspect", image).Run()
	return err == nil
}

// Pulls an image from the docker registry.
func pullDockerImage(image string) error {
	log.Printf("INFO: Pulling %s from docker registry...", image)
	return run(exec.Command("docker", "pull", image))
}

// compile cross builds a requested package according to the given build specs
// using a specific docker cross compilation image.
func compile(image string, config *ConfigFlags, flags *BuildFlags) error {
	// If a local build was requested, find the import path and mount all GOPATH sources
	locals, mounts, paths := []string{}, []string{}, []string{}
	var usesModules bool = true
	if strings.HasPrefix(config.ProjectPath, string(filepath.Separator)) || strings.HasPrefix(config.ProjectPath, ".") {
		if fileExists(filepath.Join(config.ProjectPath, "go.mod")) {
			usesModules = true
		}
		if !usesModules {
			// Resolve the repository import path from the file path
			config.ProjectPath = resolveImportPath(config.ProjectPath)
			if fileExists(filepath.Join(config.ProjectPath, "go.mod")) {
				usesModules = true
			}
		}
		if !usesModules {
			log.Println("INFO: go.mod not found. Skipping go modules")
		}

		gopathEnv := os.Getenv("GOPATH")
		if gopathEnv == "" && !usesModules {
			log.Printf("INFO: No $GOPATH is set - defaulting to %s", build.Default.GOPATH)
			gopathEnv = build.Default.GOPATH
		}

		// Iterate over all the local libs and export the mount points
		if gopathEnv == "" && !usesModules {
			log.Fatalf("INFO: No $GOPATH is set or forwarded to xgo")
		}

		if !usesModules {
			os.Setenv("GO111MODULE", "off")
			for _, gopath := range strings.Split(gopathEnv, string(os.PathListSeparator)) {
				// Since docker sandboxes volumes, resolve any symlinks manually
				sources := filepath.Join(gopath, "src")
				filepath.Walk(sources, func(path string, info os.FileInfo, err error) error {
					// Skip any folders that errored out
					if err != nil {
						log.Printf("WARNING: Failed to access GOPATH element %s: %v", path, err)
						return nil
					}
					// Skip anything that's not a symlink
					if info.Mode()&os.ModeSymlink == 0 {
						return nil
					}
					// Resolve the symlink and skip if it's not a folder
					target, err := filepath.EvalSymlinks(path)
					if err != nil {
						return nil
					}
					if info, err = os.Stat(target); err != nil || !info.IsDir() {
						return nil
					}
					// Skip if the symlink points within GOPATH
					if filepath.HasPrefix(target, sources) {
						return nil
					}

					// Folder needs explicit mounting due to docker symlink security
					locals = append(locals, target)
					mounts = append(mounts, filepath.Join("/ext-go", strconv.Itoa(len(locals)), "src", strings.TrimPrefix(path, sources)))
					paths = append(paths, filepath.ToSlash(filepath.Join("/ext-go", strconv.Itoa(len(locals)))))
					return nil
				})
				// Export the main mount point for this GOPATH entry
				locals = append(locals, sources)
				mounts = append(mounts, filepath.Join("/ext-go", strconv.Itoa(len(locals)), "src"))
				paths = append(paths, filepath.ToSlash(filepath.Join("/ext-go", strconv.Itoa(len(locals)))))
			}
		}
	}
	// Assemble and run the cross compilation command
	log.Printf("INFO: Cross compiling project %s package %s ...", config.ProjectPath, config.CmdPath)

	args := []string{
		"run", "--rm",
		"-v", config.BinPath + ":/build",
		"-v", depsCache + ":/deps-cache:ro",
		"-e", "REPO_REMOTE=" + config.Remote,
		"-e", "REPO_BRANCH=" + config.Branch,
		"-e", "PACK=" + config.Package,
		"-e", "DEPS=" + config.Dependencies,
		"-e", "ARGS=" + config.Arguments,
		"-e", "OUT=" + config.Prefix,
		"-e", fmt.Sprintf("FLAG_V=%v", flags.Verbose),
		"-e", fmt.Sprintf("FLAG_X=%v", flags.Steps),
		"-e", fmt.Sprintf("FLAG_RACE=%v", flags.Race),
		"-e", fmt.Sprintf("FLAG_TAGS=%s", flags.Tags),
		"-e", fmt.Sprintf("FLAG_LDFLAGS=%s", flags.LdFlags),
		"-e", fmt.Sprintf("FLAG_BUILDMODE=%s", flags.Mode),
		"-e", fmt.Sprintf("FLAG_BUILDVCS=%s", flags.VCS),
		"-e", fmt.Sprintf("FLAG_TRIMPATH=%v", flags.TrimPath),
		"-e", "TARGETS=" + strings.Replace(strings.Join(config.Targets, " "), "*", ".", -1),
	}
	if usesModules {
		args = append(args, []string{"-e", "GO111MODULE=on"}...)
		args = append(args, []string{"-v", build.Default.GOPATH + ":/go"}...)
		if *goProxy != "" {
			args = append(args, []string{"-e", fmt.Sprintf("GOPROXY=%s", *goProxy)}...)
		}

		// Map this repository to the /source folder
		absProjectPath, err := filepath.Abs(config.ProjectPath)
		if err != nil {
			log.Fatalf("ERROR: Failed to locate requested module repository: %v.", err)
		}
		args = append(args, []string{"-v", absProjectPath + ":/source"}...)

		// Check whether it has a vendor folder, and if so, use it
		vendorPath := absProjectPath + "/vendor"
		vendorfolder, err := os.Stat(vendorPath)
		if !os.IsNotExist(err) && vendorfolder.Mode().IsDir() {
			args = append(args, []string{"-e", "FLAG_MOD=vendor"}...)
			log.Printf("INFO: Using vendored Go module dependencies")
		}
	} else {
		args = append(args, []string{"-e", "GO111MODULE=off"}...)
		for i := 0; i < len(locals); i++ {
			args = append(args, []string{"-v", fmt.Sprintf("%s:%s:ro", locals[i], mounts[i])}...)
		}
		args = append(args, []string{"-e", "EXT_GOPATH=" + strings.Join(paths, ":")}...)
	}

	args = append(args, []string{image, config.CmdPath}...)
	log.Printf("INFO: Docker %s", strings.Join(args, " "))
	return run(exec.Command("docker", args...))
}

// compileContained cross builds a requested package according to the given build
// specs using the current system opposed to running in a container. This is meant
// to be used for cross compilation already from within an xgo image, allowing the
// inheritance and bundling of the root xgo images.
func compileContained(config *ConfigFlags, flags *BuildFlags) error {
	// If a local build was requested, resolve the import path
	local := strings.HasPrefix(config.ProjectPath, string(filepath.Separator)) || strings.HasPrefix(config.ProjectPath, ".")
	if local {
		// Resolve the repository import path from the file path
		config.ProjectPath = resolveImportPath(config.ProjectPath)

		// Determine if this is a module-based repository
		usesModules := fileExists(filepath.Join(config.ProjectPath, "go.mod"))
		if !usesModules {
			os.Setenv("GO111MODULE", "off")
			log.Println("INFO: Don't use go modules (go.mod not found)")
		}
	}
	// Fine tune the original environment variables with those required by the build script
	env := []string{
		"REPO_REMOTE=" + config.Remote,
		"REPO_BRANCH=" + config.Branch,
		"PACK=" + config.Package,
		"DEPS=" + config.Dependencies,
		"ARGS=" + config.Arguments,
		"OUT=" + config.Prefix,
		fmt.Sprintf("FLAG_V=%v", flags.Verbose),
		fmt.Sprintf("FLAG_X=%v", flags.Steps),
		fmt.Sprintf("FLAG_RACE=%v", flags.Race),
		fmt.Sprintf("FLAG_TAGS=%s", flags.Tags),
		fmt.Sprintf("FLAG_LDFLAGS=%s", flags.LdFlags),
		fmt.Sprintf("FLAG_BUILDMODE=%s", flags.Mode),
		fmt.Sprintf("FLAG_BUILDVCS=%s", flags.VCS),
		fmt.Sprintf("FLAG_TRIMPATH=%v", flags.TrimPath),
		"TARGETS=" + strings.Replace(strings.Join(config.Targets, " "), "*", ".", -1),
	}
	if local {
		env = append(env, "EXT_GOPATH=/non-existent-path-to-signal-local-build")
	}
	// Assemble and run the local cross compilation command
	log.Printf("INFO: Cross compiling project %s package %s ...", config.ProjectPath, config.CmdPath)

	cmd := exec.Command("xgo-build", config.CmdPath)
	cmd.Env = append(os.Environ(), env...)

	return run(cmd)
}

// resolveImportPath converts a package given by a relative path to a Go import
// path using the local GOPATH environment.
func resolveImportPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		log.Fatalf("ERROR: Failed to locate requested package: %v.", err)
	}
	stat, err := os.Stat(abs)
	if err != nil || !stat.IsDir() {
		log.Fatalf("ERROR: Requested path invalid.")
	}
	pack, err := build.ImportDir(abs, build.FindOnly)
	if err != nil {
		log.Fatalf("ERROR: Failed to resolve import path: %v.", err)
	}
	return pack.ImportPath
}

// Executes a command synchronously, redirecting its output to stdout.
func run(cmd *exec.Cmd) error {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// fileExists checks if given file exists
func fileExists(file string) bool {
	if _, err := os.Stat(file); os.IsNotExist(err) {
		return false
	}
	return true
}
