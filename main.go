package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/gaia-adm/pumba/action"
	"github.com/gaia-adm/pumba/container"

	"github.com/urfave/cli"

	"github.com/johntdyer/slackrus"
)

var (
	gWG       sync.WaitGroup
	client    container.Client
	chaos     action.Chaos
	gInterval time.Duration
	gTestRun  bool
	gStopChan chan bool
)

// LinuxSignals valid Linux signal table
// http://www.comptechdoc.org/os/linux/programming/linux_pgsignals.html
var LinuxSignals = map[string]int{
	"SIGHUP":    1,
	"SIGINT":    2,
	"SIGQUIT":   3,
	"SIGILL":    4,
	"SIGTRAP":   5,
	"SIGIOT":    6,
	"SIGBUS":    7,
	"SIGFPE":    8,
	"SIGKILL":   9,
	"SIGUSR1":   10,
	"SIGSEGV":   11,
	"SIGUSR2":   12,
	"SIGPIPE":   13,
	"SIGALRM":   14,
	"SIGTERM":   15,
	"SIGSTKFLT": 16,
	"SIGCHLD":   17,
	"SIGCONT":   18,
	"SIGSTOP":   19,
	"SIGTSTP":   20,
	"SIGTTIN":   21,
	"SIGTTOU":   22,
	"SIGURG":    23,
	"SIGXCPU":   24,
	"SIGXFSZ":   25,
	"SIGVTALRM": 26,
	"SIGPROF":   27,
	"SIGWINCH":  28,
	"SIGIO":     29,
	"SIGPWR":    30,
}

const (
	// Release version
	Release = "v0.2.4"
	// DefaultSignal default kill signal
	DefaultSignal = "SIGKILL"
	// Re2Prefix re2 regexp string prefix
	Re2Prefix = "re2:"
	// DefaultInterface default network interface
	DefaultInterface = "eth0"
)

func contains(slice []string, item string) bool {
	set := make(map[string]struct{}, len(slice))
	for _, s := range slice {
		set[s] = struct{}{}
	}
	_, ok := set[item]
	return ok
}

func init() {
	log.SetLevel(log.InfoLevel)
	log.SetFormatter(&log.TextFormatter{})
}

func main() {
	rootCertPath := "/etc/ssl/docker"

	if os.Getenv("DOCKER_CERT_PATH") != "" {
		rootCertPath = os.Getenv("DOCKER_CERT_PATH")
	}

	app := cli.NewApp()
	app.Name = "Pumba"
	app.Version = Release
	app.Usage = "Pumba is a resilience testing tool, that helps applications tolerate random Docker container failures: process, network and performance."
	app.ArgsUsage = "containers (name, list of names, RE2 regex)"
	app.Before = before
	app.Commands = []cli.Command{
		{
			Name: "kill",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "signal, s",
					Usage: "termination signal, that will be sent by Pumba to the main process inside target container(s)",
					Value: DefaultSignal,
				},
			},
			Usage:       "kill specified containers",
			ArgsUsage:   "containers (name, list of names, RE2 regex)",
			Description: "send termination signal to the main process inside target container(s)",
			Action:      kill,
			Before:      beforeCommand,
		},
		{
			Name: "netem",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "duration, d",
					Usage: "network emulation duration; should be smaller than recurrent interval; use with optional unit suffix: 'ms/s/m/h'",
				},
				cli.StringFlag{
					Name:  "interface, i",
					Usage: "network interface to apply delay on",
					Value: DefaultInterface,
				},
				cli.StringFlag{
					Name:  "target, t",
					Usage: "target IP filter; netem will impact only on traffic to target IP",
				},
			},
			Usage:       "emulate the properties of wide area networks",
			ArgsUsage:   "containers (name, list of names, RE2 regex)",
			Description: "delay, loss, duplicate and re-order (run 'netem') packets, to emulate different network problems",
			Subcommands: []cli.Command{
				{
					Name: "delay",
					Flags: []cli.Flag{
						cli.IntFlag{
							Name:  "time, t",
							Usage: "delay time; in milliseconds",
							Value: 100,
						},
						cli.IntFlag{
							Name:  "jitter, j",
							Usage: "random delay variation (jitter); in milliseconds; example: 100ms ± 10ms",
							Value: 10,
						},
						cli.Float64Flag{
							Name:  "correlation, c",
							Usage: "delay correlation; in percentage",
							Value: 20,
						},
						cli.StringFlag{
							Name:  "distribution, d",
							Usage: "delay distribution, can be one of {<empty> | uniform | normal | pareto |  paretonormal}",
							Value: "",
						},
					},
					Usage:       "dealy egress traffic",
					ArgsUsage:   "containers (name, list of names, RE2 regex)",
					Description: "dealy egress traffic for specified containers; networks show variability so it is possible to add random variation; delay variation isn't purely random, so to emulate that there is a correlation",
					Action:      netemDelay,
					Before:      beforeCommand,
				},
				{
					Name: "loss",
					Flags: []cli.Flag{
						cli.Float64Flag{
							Name:  "percent, p",
							Usage: "packet loss percentage",
							Value: 0.0,
						},
						cli.Float64Flag{
							Name:  "correlation, c",
							Usage: "loss correlation; in percentage",
							Value: 0.0,
						},
					},
					Usage:       "adds packet losses",
					ArgsUsage:   "containers (name, list of names, RE2 regex)",
					Description: "adds packet losses, based on independent (Bernoulli) probability model\n \tsee:  http://www.voiptroubleshooter.com/indepth/burstloss.html",
					Action:      netemLossRandom,
					Before:      beforeCommand,
				},
				{
					Name: "loss-state",
					Flags: []cli.Flag{
						cli.Float64Flag{
							Name:  "p13",
							Usage: "probability to go from state (1) to state (3)",
							Value: 0.0,
						},
						cli.Float64Flag{
							Name:  "p31",
							Usage: "probability to go from state (3) to state (1)",
							Value: 100.0,
						},
						cli.Float64Flag{
							Name:  "p32",
							Usage: "probability to go from state (3) to state (2)",
							Value: 0.0,
						},
						cli.Float64Flag{
							Name:  "p23",
							Usage: "probability to go from state (2) to state (3)",
							Value: 100.0,
						},
						cli.Float64Flag{
							Name:  "p14",
							Usage: "probability to go from state (1) to state (4)",
							Value: 0.0,
						},
					},
					Usage:       "adds packet losses, based on 4-state Markov probability model",
					ArgsUsage:   "containers (name, list of names, RE2 regex)",
					Description: "adds a packet losses, based on 4-state Markov probability model\n \t\tstate (1) – packet received successfully\n \t\tstate (2) – packet received within a burst\n \t\tstate (3) – packet lost within a burst\n \t\tstate (4) – isolated packet lost within a gap\n \tsee: http://www.voiptroubleshooter.com/indepth/burstloss.html",
					Action:      netemLossState,
					Before:      beforeCommand,
				},
				{
					Name: "loss-gemodel",
					Flags: []cli.Flag{
						cli.Float64Flag{
							Name:  "pg, p",
							Usage: "transition probability into the bad state",
							Value: 0.0,
						},
						cli.Float64Flag{
							Name:  "pb, r",
							Usage: "transition probability into the good state",
							Value: 100.0,
						},
						cli.Float64Flag{
							Name:  "one-h",
							Usage: "loss probability in the bad state",
							Value: 100.0,
						},
						cli.Float64Flag{
							Name:  "one-k",
							Usage: "loss probability in the good state",
							Value: 0.0,
						},
					},
					Usage:       "adds packet losses, according to the Gilbert-Elliot loss model",
					ArgsUsage:   "containers (name, list of names, RE2 regex)",
					Description: "adds packet losses, according to the Gilbert-Elliot loss model\n \tsee: http://www.voiptroubleshooter.com/indepth/burstloss.html",
					Action:      netemLossGEmodel,
					Before:      beforeCommand,
				},
				{
					Name: "duplicate",
				},
				{
					Name: "corrupt",
				},
			},
		},
		{
			Name: "pause",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "duration, d",
					Usage: "pause duration: should be smaller than recurrent interval; use with optional unit suffix: 'ms/s/m/h'",
				},
			},
			Usage:       "pause all processes",
			ArgsUsage:   "containers (name, list of names, RE2 regex)",
			Description: "pause all running processes within target containers",
			Action:      pause,
			Before:      beforeCommand,
		},
		{
			Name: "stop",
			Flags: []cli.Flag{
				cli.IntFlag{
					Name:  "time, t",
					Usage: "seconds to wait for stop before killing container (default 10)",
					Value: 10,
				},
			},
			Usage:       "stop containers",
			ArgsUsage:   "containers (name, list of names, RE2 regex)",
			Description: "stop the main process inside target containers, sending  SIGTERM, and then SIGKILL after a grace period",
			Action:      stop,
			Before:      beforeCommand,
		},
		{
			Name: "rm",
			Flags: []cli.Flag{
				cli.BoolTFlag{
					Name:  "force, f",
					Usage: "force the removal of a running container (with SIGKILL)",
				},
				cli.BoolTFlag{
					Name:  "links, l",
					Usage: "remove container links",
				},
				cli.BoolTFlag{
					Name:  "volumes, v",
					Usage: "remove volumes associated with the container",
				},
			},
			Usage:       "remove containers",
			ArgsUsage:   "containers (name, list of names, RE2 regex)",
			Description: "remove target containers, with links and voluems",
			Action:      remove,
			Before:      beforeCommand,
		},
	}
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "host, H",
			Usage:  "daemon socket to connect to",
			Value:  "unix:///var/run/docker.sock",
			EnvVar: "DOCKER_HOST",
		},
		cli.BoolFlag{
			Name:  "tls",
			Usage: "use TLS; implied by --tlsverify",
		},
		cli.BoolFlag{
			Name:   "tlsverify",
			Usage:  "use TLS and verify the remote",
			EnvVar: "DOCKER_TLS_VERIFY",
		},
		cli.StringFlag{
			Name:  "tlscacert",
			Usage: "trust certs signed only by this CA",
			Value: fmt.Sprintf("%s/ca.pem", rootCertPath),
		},
		cli.StringFlag{
			Name:  "tlscert",
			Usage: "client certificate for TLS authentication",
			Value: fmt.Sprintf("%s/cert.pem", rootCertPath),
		},
		cli.StringFlag{
			Name:  "tlskey",
			Usage: "client key for TLS authentication",
			Value: fmt.Sprintf("%s/key.pem", rootCertPath),
		},
		cli.BoolFlag{
			Name:  "debug",
			Usage: "enable debug mode with verbose logging",
		},
		cli.BoolFlag{
			Name:  "json",
			Usage: "produce log in JSON format: Logstash and Splunk friendly"},
		cli.StringFlag{
			Name:  "slackhook",
			Usage: "web hook url; send Pumba log events to Slack",
		},
		cli.StringFlag{
			Name:  "slackchannel",
			Usage: "Slack channel (default #pumba)",
			Value: "#pumba",
		},
		cli.StringFlag{
			Name:  "interval, i",
			Usage: "recurrent interval for chaos command; use with optional unit suffix: 'ms/s/m/h'",
		},
		cli.BoolFlag{
			Name:        "random, r",
			Usage:       "randomly select single matching container from list of target containers",
			Destination: &action.RandomMode,
		},
		cli.BoolFlag{
			Name:        "dry",
			Usage:       "dry runl does not create chaos, only logs planned chaos commands",
			Destination: &action.DryMode,
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func before(c *cli.Context) error {
	// set debug log level
	if c.GlobalBool("debug") {
		log.SetLevel(log.DebugLevel)
	}
	// set log formatter to JSON
	if c.GlobalBool("json") {
		log.SetFormatter(&log.JSONFormatter{})
	}
	// set Slack log channel
	if c.GlobalString("slackhook") != "" {
		log.AddHook(&slackrus.SlackrusHook{
			HookURL:        c.GlobalString("slackhook"),
			AcceptedLevels: slackrus.LevelThreshold(log.GetLevel()),
			Channel:        c.GlobalString("slackchannel"),
			IconEmoji:      ":boar:",
			Username:       "pumba_bot",
		})
	}
	// Set-up container client
	tls, err := tlsConfig(c)
	if err != nil {
		return err
	}
	// create new Docker client
	client = container.NewClient(c.GlobalString("host"), tls)
	// create new Chaos instance
	chaos = action.NewChaos()
	// habdle termination signal
	handleSignals()
	return nil
}

// beforeCommand run before each chaos command
func beforeCommand(c *cli.Context) error {
	// get recurrent time interval
	if intervalString := c.GlobalString("interval"); intervalString == "" {
		log.Debug("No interval, running only once")
	} else if interval, err := time.ParseDuration(intervalString); err != nil {
		return err
	} else {
		gInterval = interval
	}
	return nil
}

func getNamesOrPattern(c *cli.Context) ([]string, string) {
	names := []string{}
	pattern := ""
	// get container names or pattern: no Args means ALL containers
	if c.Args().Present() {
		// more than one argument, assume that this a list of names
		if len(c.Args()) > 1 {
			names = c.Args()
			log.Debugf("Names: '%s'", names)
		} else {
			first := c.Args().First()
			if strings.HasPrefix(first, Re2Prefix) {
				pattern = strings.Trim(first, Re2Prefix)
				log.Debugf("Pattern: '%s'", pattern)
			} else {
				names = append(names, first)
			}
		}
	}
	return names, pattern
}

func runChaosCommand(cmd interface{}, names []string, pattern string, chaosFn func(container.Client, []string, string, interface{}) error) {
	// channel for 'chaos' command
	dc := make(chan interface{})
	// create Time channel for specified intterval: for TestRun use Timer (one time call)
	var cmdTimeChan <-chan time.Time
	if gInterval == 0 || gTestRun {
		cmdTimeChan = time.NewTimer(gInterval).C
	} else {
		cmdTimeChan = time.NewTicker(gInterval).C
	}
	// handle interval timer event
	go func(cmd interface{}) {
		for range cmdTimeChan {
			dc <- cmd
		}
	}(cmd)
	// handle 'chaos' command
	for cmd := range dc {
		gWG.Add(1)
		go func(cmd interface{}) {
			defer gWG.Done()
			if err := chaosFn(client, names, pattern, cmd); err != nil {
				log.Error(err)
			}
			if gInterval == 0 || gTestRun {
				close(dc)
			}
		}(cmd)
	}
}

// KILL Command
func kill(c *cli.Context) error {
	// get names or pattern
	names, pattern := getNamesOrPattern(c)
	// get signal
	signal := c.String("signal")
	if _, ok := LinuxSignals[signal]; !ok {
		err := errors.New("Unexpected signal: " + signal)
		log.Error(err)
		return err
	}
	runChaosCommand(action.CommandKill{Signal: signal}, names, pattern, chaos.KillContainers)
	return nil
}

func parseNetemOptions(c *cli.Context) ([]string, string, time.Duration, string, net.IP, error) {
	// get names or pattern
	names, pattern := getNamesOrPattern(c)
	// get duration
	var durationString string
	if c.Parent() != nil {
		durationString = c.Parent().String("duration")
	}
	if durationString == "" {
		err := errors.New("Undefined duration interval")
		log.Error(err)
		return names, pattern, 0, "", nil, err
	}
	duration, err := time.ParseDuration(durationString)
	if err != nil {
		log.Error(err)
		return names, pattern, 0, "", nil, err
	}
	if gInterval != 0 && duration >= gInterval {
		err = errors.New("Duration cannot be bigger than interval")
		log.Error(err)
		return names, pattern, 0, "", nil, err
	}
	// get network interface and target ip
	netInterface := DefaultInterface
	var ip net.IP
	if c.Parent() != nil {
		netInterface = c.Parent().String("interface")
		// protect from Command Injection, using Regexp
		reInterface := regexp.MustCompile("[a-zA-Z]+[0-9]{0,2}")
		validInterface := reInterface.FindString(netInterface)
		if netInterface != validInterface {
			err = fmt.Errorf("Bad network interface name. Must match '%s'", reInterface.String())
			log.Error(err)
			return names, pattern, duration, "", nil, err
		}
		// get target IP Filter
		ip = net.ParseIP(c.Parent().String("target"))
	}
	return names, pattern, duration, netInterface, ip, nil
}

// NETEM DELAY command
func netemDelay(c *cli.Context) error {
	// parse common netem options
	names, pattern, duration, netInterface, ip, err := parseNetemOptions(c)
	if err != nil {
		return err
	}
	// get delay time
	time := c.Int("time")
	if time <= 0 {
		err = errors.New("Invalid delay time")
		log.Error(err)
		return err
	}
	// get delay variation
	jitter := c.Int("jitter")
	if jitter < 0 || jitter > time {
		err = errors.New("Invalid delay jitter")
		log.Error(err)
		return err
	}
	// get delay variation
	correlation := c.Float64("correlation")
	if correlation < 0.0 || correlation > 100.0 {
		err = errors.New("Invalid delay correlation: must be between 0.0 and 100.0")
		log.Error(err)
		return err
	}
	// get distribution
	distribution := c.String("distribution")
	if ok := contains(action.DelayDistribution, distribution); !ok {
		err = errors.New("Invalid delay distribution: must be one of {uniform | normal | pareto |  paretonormal}")
		log.Error(err)
		return err
	}
	// pepare netem delay command
	delayCmd := action.CommandNetemDelay{
		NetInterface: netInterface,
		IP:           ip,
		Duration:     duration,
		Time:         time,
		Jitter:       jitter,
		Correlation:  correlation,
		Distribution: distribution,
		StopChan:     gStopChan,
	}
	runChaosCommand(delayCmd, names, pattern, chaos.NetemDelayContainers)
	return nil
}

// NETEM LOSS random command
func netemLossRandom(c *cli.Context) error {
	// parse common netem options
	names, pattern, duration, netInterface, ip, err := parseNetemOptions(c)
	if err != nil {
		return err
	}
	// get loss percentage
	percent := c.Float64("percent")
	if percent < 0.0 || percent > 100.0 {
		err = errors.New("Invalid packet loss percentage: : must be between 0 and 100")
		log.Error(err)
		return err
	}
	// get delay variation
	correlation := c.Float64("correlation")
	if correlation < 0.0 || correlation > 100.0 {
		err = errors.New("Invalid loss correlation: must be between 0 and 100")
		log.Error(err)
		return err
	}
	// pepare netem loss command
	delayCmd := action.CommandNetemLossRandom{
		NetInterface: netInterface,
		IP:           ip,
		Duration:     duration,
		Percent:      percent,
		Correlation:  correlation,
		StopChan:     gStopChan,
	}
	runChaosCommand(delayCmd, names, pattern, chaos.NetemLossRandomContainers)
	return nil
}

// NETEM LOSS state command
func netemLossState(c *cli.Context) error {
	// parse common netem options
	names, pattern, duration, netInterface, ip, err := parseNetemOptions(c)
	if err != nil {
		return err
	}
	// get p13
	p13 := c.Float64("p13")
	if p13 < 0.0 || p13 > 100.0 {
		err = errors.New("Invalid p13 percentage: : must be between 0.0 and 100.0")
		log.Error(err)
		return err
	}
	// get p31
	p31 := c.Float64("p31")
	if p31 < 0.0 || p31 > 100.0 {
		err = errors.New("Invalid p31 percentage: : must be between 0.0 and 100.0")
		log.Error(err)
		return err
	}
	// get p32
	p32 := c.Float64("p32")
	if p32 < 0.0 || p32 > 100.0 {
		err = errors.New("Invalid p32 percentage: : must be between 0.0 and 100.0")
		log.Error(err)
		return err
	}
	// get p23
	p23 := c.Float64("p23")
	if p23 < 0.0 || p23 > 100.0 {
		err = errors.New("Invalid p23 percentage: : must be between 0.0 and 100.0")
		log.Error(err)
		return err
	}
	// get p14
	p14 := c.Float64("p14")
	if p14 < 0.0 || p14 > 100.0 {
		err = errors.New("Invalid p14 percentage: : must be between 0.0 and 100.0")
		log.Error(err)
		return err
	}
	// pepare netem loss command
	delayCmd := action.CommandNetemLossState{
		NetInterface: netInterface,
		IP:           ip,
		Duration:     duration,
		P13:          p13,
		P31:          p31,
		P32:          p32,
		P23:          p23,
		P14:          p14,
		StopChan:     gStopChan,
	}
	runChaosCommand(delayCmd, names, pattern, chaos.NetemLossStateContainers)
	return nil
}

// NETEM Gilbert-Elliot command
func netemLossGEmodel(c *cli.Context) error {
	// parse common netem options
	names, pattern, duration, netInterface, ip, err := parseNetemOptions(c)
	if err != nil {
		return err
	}
	// get pg - Good State transition probability
	pg := c.Float64("pg")
	if pg < 0.0 || pg > 100.0 {
		err = errors.New("Invalid pg (Good State) transition probability: must be between 0.0 and 100.0")
		log.Error(err)
		return err
	}
	// get pb - Bad State transition probability
	pb := c.Float64("pb")
	if pb < 0.0 || pb > 100.0 {
		err = errors.New("Invalid pb (Bad State) transition probability: must be between 0.0 and 100.0")
		log.Error(err)
		return err
	}
	// get (1-h) - loss probability in Bad state
	oneH := c.Float64("one-h")
	if oneH < 0.0 || oneH > 100.0 {
		err = errors.New("Invalid loss probability: must be between 0.0 and 100.0")
		log.Error(err)
		return err
	}
	// get (1-k) - loss probability in Good state
	oneK := c.Float64("one-k")
	if oneK < 0.0 || oneK > 100.0 {
		err = errors.New("Invalid loss probability: must be between 0.0 and 100.0")
		log.Error(err)
		return err
	}
	// pepare netem loss command
	delayCmd := action.CommandNetemLossGEmodel{
		NetInterface: netInterface,
		IP:           ip,
		Duration:     duration,
		PG:           pg,
		PB:           pb,
		OneH:         oneH,
		OneK:         oneK,
		StopChan:     gStopChan,
	}
	runChaosCommand(delayCmd, names, pattern, chaos.NetemLossGEmodelContainers)
	return nil
}

// PAUSE command
func pause(c *cli.Context) error {
	// get names or pattern
	names, pattern := getNamesOrPattern(c)
	// get duration
	durationString := c.String("duration")
	if durationString == "" {
		err := errors.New("Undefined duration interval")
		log.Error(err)
		return err
	}
	duration, err := time.ParseDuration(durationString)
	if err != nil {
		log.Error(err)
		return err
	}
	cmd := action.CommandPause{
		Duration: duration,
		StopChan: gStopChan,
	}
	runChaosCommand(cmd, names, pattern, chaos.PauseContainers)
	return nil
}

// REMOVE Command
func remove(c *cli.Context) error {
	// get names or pattern
	names, pattern := getNamesOrPattern(c)
	// get force flag
	force := c.BoolT("force")
	// get link flag
	links := c.BoolT("links")
	// get link flag
	volumes := c.BoolT("volumes")
	// run chaos command
	cmd := action.CommandRemove{Force: force, Links: links, Volumes: volumes}
	runChaosCommand(cmd, names, pattern, chaos.RemoveContainers)
	return nil
}

// STOP Command
func stop(c *cli.Context) error {
	// get names or pattern
	names, pattern := getNamesOrPattern(c)
	// run chaos command
	cmd := action.CommandStop{WaitTime: c.Int("time")}
	runChaosCommand(cmd, names, pattern, chaos.StopContainers)
	return nil
}

func handleSignals() {
	// Graceful shut-down on SIGINT/SIGTERM
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	// channel to notify long running commands to stop and cleanup
	// long running commands must listen to this channel and react
	gStopChan = make(chan bool, 1)

	go func() {
		sid := <-sigs
		log.Debugf("Recieved signal: %d", sid)
		gStopChan <- true
		log.Debug("Sending stop signal to runnung chaos commands ...")
		gWG.Wait()
		log.Debug("Graceful exit :-)")
		//os.Exit(1)
	}()
}

// tlsConfig translates the command-line options into a tls.Config struct
func tlsConfig(c *cli.Context) (*tls.Config, error) {
	var tlsConfig *tls.Config
	var err error
	caCertFlag := c.GlobalString("tlscacert")
	certFlag := c.GlobalString("tlscert")
	keyFlag := c.GlobalString("tlskey")

	if c.GlobalBool("tls") || c.GlobalBool("tlsverify") {
		tlsConfig = &tls.Config{
			InsecureSkipVerify: !c.GlobalBool("tlsverify"),
		}

		// Load CA cert
		if caCertFlag != "" {
			var caCert []byte
			if strings.HasPrefix(caCertFlag, "/") {
				caCert, err = ioutil.ReadFile(caCertFlag)
				if err != nil {
					return nil, err
				}
			} else {
				caCert = []byte(caCertFlag)
			}
			caCertPool := x509.NewCertPool()
			caCertPool.AppendCertsFromPEM(caCert)
			tlsConfig.RootCAs = caCertPool
		}

		// Load client certificate
		if certFlag != "" && keyFlag != "" {
			var cert tls.Certificate
			if strings.HasPrefix(certFlag, "/") && strings.HasPrefix(keyFlag, "/") {
				cert, err = tls.LoadX509KeyPair(certFlag, keyFlag)
				if err != nil {
					return nil, err
				}
			} else {
				cert, err = tls.X509KeyPair([]byte(certFlag), []byte(keyFlag))
				if err != nil {
					return nil, err
				}
			}
			tlsConfig.Certificates = []tls.Certificate{cert}
		}
	}
	return tlsConfig, nil
}
