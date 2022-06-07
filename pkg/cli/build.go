package cli

// the cli package contains urfave/cli related structs that help make up
// the command line for buildah commands. it resides here so other projects
// that vendor in this code can use them too.

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containers/buildah/define"
	"github.com/containers/buildah/pkg/parse"
	"github.com/containers/buildah/pkg/util"
	"github.com/containers/common/pkg/auth"
	encconfig "github.com/containers/ocicrypt/config"
	enchelpers "github.com/containers/ocicrypt/helpers"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

type BuildOptions struct {
	*LayerResults
	*BudResults
	*UserNSResults
	*FromAndBudResults
	*NameSpaceResults
	Logwriter *os.File
}

const (
	MaxPullPushRetries = 3
	PullPushRetryDelay = 2 * time.Second
)

// GenBuildOptions translates command line flags into a BuildOptions structure
func GenBuildOptions(c *cobra.Command, inputArgs []string, iopts BuildOptions) (define.BuildOptions, []string, []string, error) {
	options := define.BuildOptions{}

	var removeAll []string

	output := ""
	cleanTmpFile := false
	tags := []string{}
	if c.Flag("tag").Changed {
		tags = iopts.Tag
		if len(tags) > 0 {
			output = tags[0]
			tags = tags[1:]
		}
		if c.Flag("manifest").Changed {
			for _, tag := range tags {
				if tag == iopts.Manifest {
					return options, nil, nil, errors.New("the same name must not be specified for both '--tag' and '--manifest'")
				}
			}
		}
	}
	if err := auth.CheckAuthFile(iopts.BudResults.Authfile); err != nil {
		return options, nil, nil, err
	}

	if c.Flag("logsplit").Changed {
		if !c.Flag("logfile").Changed {
			return options, nil, nil, errors.Errorf("cannot use --logsplit without --logfile")
		}
	}

	iopts.BudResults.Authfile, cleanTmpFile = util.MirrorToTempFileIfPathIsDescriptor(iopts.BudResults.Authfile)
	if cleanTmpFile {
		removeAll = append(removeAll, iopts.BudResults.Authfile)
	}

	// Allow for --pull, --pull=true, --pull=false, --pull=never, --pull=always
	// --pull-always and --pull-never.  The --pull-never and --pull-always options
	// will not be documented.
	pullPolicy := define.PullIfMissing
	if strings.EqualFold(strings.TrimSpace(iopts.Pull), "true") {
		pullPolicy = define.PullIfNewer
	}
	if iopts.PullAlways || strings.EqualFold(strings.TrimSpace(iopts.Pull), "always") {
		pullPolicy = define.PullAlways
	}
	if iopts.PullNever || strings.EqualFold(strings.TrimSpace(iopts.Pull), "never") {
		pullPolicy = define.PullNever
	}
	logrus.Debugf("Pull Policy for pull [%v]", pullPolicy)

	args := make(map[string]string)
	if c.Flag("build-arg").Changed {
		for _, arg := range iopts.BuildArg {
			av := strings.SplitN(arg, "=", 2)
			if len(av) > 1 {
				args[av[0]] = av[1]
			} else {
				// check if the env is set in the local environment and use that value if it is
				if val, present := os.LookupEnv(av[0]); present {
					args[av[0]] = val
				} else {
					delete(args, av[0])
				}
			}
		}
	}

	additionalBuildContext := make(map[string]*define.AdditionalBuildContext)
	if c.Flag("build-context").Changed {
		for _, contextString := range iopts.BuildContext {
			av := strings.SplitN(contextString, "=", 2)
			if len(av) > 1 {
				parseAdditionalBuildContext, err := parse.GetAdditionalBuildContext(av[1])
				if err != nil {
					return options, nil, nil, errors.Wrapf(err, "while parsing additional build context")
				}
				additionalBuildContext[av[0]] = &parseAdditionalBuildContext
			} else {
				return options, nil, nil, fmt.Errorf("while parsing additional build context: %q, accepts value in the form of key=value", av)
			}
		}
	}

	containerfiles := getContainerfiles(iopts.File)
	format, err := GetFormat(iopts.Format)
	if err != nil {
		return options, nil, nil, err
	}
	layers := UseLayers()
	if c.Flag("layers").Changed {
		layers = iopts.Layers
	}
	contextDir := ""
	cliArgs := inputArgs

	// Nothing provided, we assume the current working directory as build
	// context
	if len(cliArgs) == 0 {
		contextDir, err = os.Getwd()
		if err != nil {
			return options, nil, nil, errors.Wrapf(err, "unable to choose current working directory as build context")
		}
	} else {
		// The context directory could be a URL.  Try to handle that.
		tempDir, subDir, err := define.TempDirForURL("", "buildah", cliArgs[0])
		if err != nil {
			return options, nil, nil, errors.Wrapf(err, "error prepping temporary context directory")
		}
		if tempDir != "" {
			// We had to download it to a temporary directory.
			// Delete it later.
			removeAll = append(removeAll, tempDir)
			contextDir = filepath.Join(tempDir, subDir)
		} else {
			// Nope, it was local.  Use it as is.
			absDir, err := filepath.Abs(cliArgs[0])
			if err != nil {
				return options, nil, nil, errors.Wrapf(err, "error determining path to directory")
			}
			contextDir = absDir
		}
	}

	if len(containerfiles) == 0 {
		// Try to find the Containerfile/Dockerfile within the contextDir
		containerfile, err := util.DiscoverContainerfile(contextDir)
		if err != nil {
			return options, nil, nil, err
		}
		containerfiles = append(containerfiles, containerfile)
		contextDir = filepath.Dir(containerfile)
	}

	contextDir, err = filepath.EvalSymlinks(contextDir)
	if err != nil {
		return options, nil, nil, errors.Wrapf(err, "error evaluating symlinks in build context path")
	}

	var stdin io.Reader
	if iopts.Stdin {
		stdin = os.Stdin
	}

	var stdout, stderr, reporter *os.File
	stdout = os.Stdout
	stderr = os.Stderr
	reporter = os.Stderr
	if iopts.Logwriter != nil {
		logrus.SetOutput(iopts.Logwriter)
		stdout = iopts.Logwriter
		stderr = iopts.Logwriter
		reporter = iopts.Logwriter
	}

	systemContext, err := parse.SystemContextFromOptions(c)
	if err != nil {
		return options, nil, nil, errors.Wrapf(err, "error building system context")
	}

	isolation, err := parse.IsolationOption(iopts.Isolation)
	if err != nil {
		return options, nil, nil, err
	}

	runtimeFlags := []string{}
	for _, arg := range iopts.RuntimeFlags {
		runtimeFlags = append(runtimeFlags, "--"+arg)
	}

	commonOpts, err := parse.CommonBuildOptions(c)
	if err != nil {
		return options, nil, nil, err
	}

	pullFlagsCount := 0
	if c.Flag("pull").Changed {
		pullFlagsCount++
	}
	if c.Flag("pull-always").Changed {
		pullFlagsCount++
	}
	if c.Flag("pull-never").Changed {
		pullFlagsCount++
	}

	if pullFlagsCount > 1 {
		return options, nil, nil, errors.Errorf("can only set one of 'pull' or 'pull-always' or 'pull-never'")
	}

	if (c.Flag("rm").Changed || c.Flag("force-rm").Changed) && (!c.Flag("layers").Changed && !c.Flag("no-cache").Changed) {
		return options, nil, nil, errors.Errorf("'rm' and 'force-rm' can only be set with either 'layers' or 'no-cache'")
	}

	if c.Flag("cache-from").Changed {
		logrus.Debugf("build --cache-from not enabled, has no effect")
	}

	if c.Flag("compress").Changed {
		logrus.Debugf("--compress option specified but is ignored")
	}

	compression := define.Gzip
	if iopts.DisableCompression {
		compression = define.Uncompressed
	}

	if c.Flag("disable-content-trust").Changed {
		logrus.Debugf("--disable-content-trust option specified but is ignored")
	}

	namespaceOptions, networkPolicy, err := parse.NamespaceOptions(c)
	if err != nil {
		return options, nil, nil, err
	}
	usernsOption, idmappingOptions, err := parse.IDMappingOptions(c, isolation)
	if err != nil {
		return options, nil, nil, errors.Wrapf(err, "error parsing ID mapping options")
	}
	namespaceOptions.AddOrReplace(usernsOption...)

	platforms, err := parse.PlatformsFromOptions(c)
	if err != nil {
		return options, nil, nil, err
	}

	decryptConfig, err := DecryptConfig(iopts.DecryptionKeys)
	if err != nil {
		return options, nil, nil, errors.Wrapf(err, "unable to obtain decrypt config")
	}

	var excludes []string
	if iopts.IgnoreFile != "" {
		if excludes, _, err = parse.ContainerIgnoreFile(contextDir, iopts.IgnoreFile); err != nil {
			return options, nil, nil, err
		}
	}
	var timestamp *time.Time
	if c.Flag("timestamp").Changed {
		t := time.Unix(iopts.Timestamp, 0).UTC()
		timestamp = &t
	}
	if c.Flag("output").Changed {
		buildOption, err := parse.GetBuildOutput(iopts.BuildOutput)
		if err != nil {
			return options, nil, nil, err
		}
		if buildOption.IsStdout {
			iopts.Quiet = true
		}
	}
	options = define.BuildOptions{
		AddCapabilities:         iopts.CapAdd,
		AdditionalTags:          tags,
		AllPlatforms:            iopts.AllPlatforms,
		Annotations:             iopts.Annotation,
		Architecture:            systemContext.ArchitectureChoice,
		Args:                    args,
		AdditionalBuildContexts: additionalBuildContext,
		BlobDirectory:           iopts.BlobCache,
		CNIConfigDir:            iopts.CNIConfigDir,
		CNIPluginPath:           iopts.CNIPlugInPath,
		CommonBuildOpts:         commonOpts,
		Compression:             compression,
		ConfigureNetwork:        networkPolicy,
		ContextDirectory:        contextDir,
		CPPFlags:                iopts.CPPFlags,
		Devices:                 iopts.Devices,
		DropCapabilities:        iopts.CapDrop,
		Err:                     stderr,
		ForceRmIntermediateCtrs: iopts.ForceRm,
		From:                    iopts.From,
		IDMappingOptions:        idmappingOptions,
		IIDFile:                 iopts.Iidfile,
		In:                      stdin,
		Isolation:               isolation,
		IgnoreFile:              iopts.IgnoreFile,
		Labels:                  iopts.Label,
		Layers:                  layers,
		LogFile:                 iopts.Logfile,
		LogSplitByPlatform:      iopts.LogSplitByPlatform,
		LogRusage:               iopts.LogRusage,
		Manifest:                iopts.Manifest,
		MaxPullPushRetries:      MaxPullPushRetries,
		NamespaceOptions:        namespaceOptions,
		NoCache:                 iopts.NoCache,
		OS:                      systemContext.OSChoice,
		Out:                     stdout,
		Output:                  output,
		BuildOutput:             iopts.BuildOutput,
		OutputFormat:            format,
		PullPolicy:              pullPolicy,
		PullPushRetryDelay:      PullPushRetryDelay,
		Quiet:                   iopts.Quiet,
		RemoveIntermediateCtrs:  iopts.Rm,
		ReportWriter:            reporter,
		Runtime:                 iopts.Runtime,
		RuntimeArgs:             runtimeFlags,
		RusageLogFile:           iopts.RusageLogFile,
		SignBy:                  iopts.SignBy,
		SignaturePolicyPath:     iopts.SignaturePolicy,
		Squash:                  iopts.Squash,
		SystemContext:           systemContext,
		Target:                  iopts.Target,
		TransientMounts:         iopts.Volumes,
		OciDecryptConfig:        decryptConfig,
		Jobs:                    &iopts.Jobs,
		Excludes:                excludes,
		Timestamp:               timestamp,
		Platforms:               platforms,
		UnsetEnvs:               iopts.UnsetEnvs,
		Envs:                    iopts.Envs,
		OSFeatures:              iopts.OSFeatures,
		OSVersion:               iopts.OSVersion,
	}
	if iopts.Quiet {
		options.ReportWriter = ioutil.Discard
	}
	return options, containerfiles, removeAll, nil
}

func getContainerfiles(files []string) []string {
	var containerfiles []string
	for _, f := range files {
		if f == "-" {
			containerfiles = append(containerfiles, "/dev/stdin")
		} else {
			containerfiles = append(containerfiles, f)
		}
	}
	return containerfiles
}

// GetFormat translates format string into either docker or OCI format constant
func GetFormat(format string) (string, error) {
	switch format {
	case define.OCI:
		return define.OCIv1ImageManifest, nil
	case define.DOCKER:
		return define.Dockerv2ImageManifest, nil
	default:
		return "", errors.Errorf("unrecognized image type %q", format)
	}
}

// DecryptConfig translates decryptionKeys into a DescriptionConfig structure
func DecryptConfig(decryptionKeys []string) (*encconfig.DecryptConfig, error) {
	decryptConfig := &encconfig.DecryptConfig{}
	if len(decryptionKeys) > 0 {
		// decryption
		dcc, err := enchelpers.CreateCryptoConfig([]string{}, decryptionKeys)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid decryption keys")
		}
		cc := encconfig.CombineCryptoConfigs([]encconfig.CryptoConfig{dcc})
		decryptConfig = cc.DecryptConfig
	}

	return decryptConfig, nil
}

// EncryptConfig translates encryptionKeys into a EncriptionsConfig structure
func EncryptConfig(encryptionKeys []string, encryptLayers []int) (*encconfig.EncryptConfig, *[]int, error) {
	var encLayers *[]int
	var encConfig *encconfig.EncryptConfig

	if len(encryptionKeys) > 0 {
		// encryption
		encLayers = &encryptLayers
		ecc, err := enchelpers.CreateCryptoConfig(encryptionKeys, []string{})
		if err != nil {
			return nil, nil, errors.Wrapf(err, "invalid encryption keys")
		}
		cc := encconfig.CombineCryptoConfigs([]encconfig.CryptoConfig{ecc})
		encConfig = cc.EncryptConfig
	}
	return encConfig, encLayers, nil
}