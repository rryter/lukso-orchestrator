package main

import (
	"fmt"
	joonix "github.com/joonix/log"
	"github.com/lukso-network/lukso-orchestrator/shared/cmd"
	"github.com/lukso-network/lukso-orchestrator/shared/journald"
	"github.com/lukso-network/lukso-orchestrator/shared/logutil"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	prefixed "github.com/x-cray/logrus-prefixed-formatter"
	"os"
	"runtime"
	runtimeDebug "runtime/debug"
)

// ANYBODY HAS THE BETTER NAME JUST GIVE PROPOSAL!

// This library is responsible to spin your lukso infrastructure (Pandora, Vanguard, Validator, Orchestrator)
// In Tolkien's stories, Celebrimbor is an elven-smith who was manipulated into forging the Rings of Power
// by the disguised villain Sauron. While Celebrimbor created a set of Three on his own,
// Sauron left for Mordor and forged the One Ring, a master ring to control all the others, in the fires of Mount Doom.
// https://en.wikipedia.org/wiki/Celebrimbor
// We want to spin also 3 libraries at once, and secretly rule them by orchestrator. It matches for me somehow

// This binary will also support only some of the possible networks.
// Make a pull request to attach your network.
// We are also very open to any improvements. Please make some issue or hackmd proposal to make it better.
// Join our lukso discord https://discord.gg/E2rJPP4 to ask some questions

var (
	appName             = "celebrimbor"
	ethstatsCredentials string
	nickname            string
	operatingSystem     string
	pandoraTag          string
	vanguardTag         string
	orchestratorTag     string
	log                 = logrus.WithField("prefix", appName)

	pandoraRuntimeFlags []string
)

func init() {
	flags := append(appFlags, pandoraFlags...)
	appFlags = cmd.WrapFlags(flags)
}

func main() {
	app := cli.App{}
	app.Name = appName
	app.Usage = "Spins all lukso ecosystem components"
	app.Flags = appFlags
	app.Action = downloadAndRunApps

	app.Before = func(ctx *cli.Context) error {
		format := ctx.String(cmd.LogFormat.Name)
		switch format {
		case "text":
			formatter := new(prefixed.TextFormatter)
			formatter.TimestampFormat = "2006-01-02 15:04:05"
			formatter.FullTimestamp = true
			// If persistent log files are written - we disable the log messages coloring because
			// the colors are ANSI codes and seen as gibberish in the log files.
			formatter.DisableColors = ctx.String(cmd.LogFileName.Name) != ""
			logrus.SetFormatter(formatter)
		case "fluentd":
			f := joonix.NewFormatter()
			if err := joonix.DisableTimestampFormat(f); err != nil {
				panic(err)
			}
			logrus.SetFormatter(f)
		case "json":
			logrus.SetFormatter(&logrus.JSONFormatter{})
		case "journald":
			if err := journald.Enable(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown log format %s", format)
		}

		logFileName := ctx.String(cmd.LogFileName.Name)
		if logFileName != "" {
			if err := logutil.ConfigurePersistentLogging(logFileName); err != nil {
				log.WithError(err).Error("Failed to configuring logging to disk.")
			}
		}

		runtime.GOMAXPROCS(runtime.NumCPU())

		pandoraRuntimeFlags = preparePandoraFlags(ctx)
		pandoraTag = ctx.String(pandoraTagFlag)

		return nil
	}

	defer func() {
		if x := recover(); x != nil {
			log.Errorf("Runtime panic: %v\n%v", x, string(runtimeDebug.Stack()))
			panic(x)
		}
	}()

	err := app.Run(os.Args)

	if nil != err {
		log.Error(err.Error())
	}
}

func downloadAndRunApps(ctx *cli.Context) (err error) {
	// Get os, then download all binaries into datadir matching desired system
	// After successful download run binary with desired arguments spin and connect them

	return
}
