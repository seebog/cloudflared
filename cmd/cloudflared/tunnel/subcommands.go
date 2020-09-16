package tunnel

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"
	"golang.org/x/net/idna"
	"gopkg.in/yaml.v2"

	"github.com/cloudflare/cloudflared/cmd/cloudflared/cliutil"
	"github.com/cloudflare/cloudflared/logger"
	"github.com/cloudflare/cloudflared/tunnelrpc/pogs"
	"github.com/cloudflare/cloudflared/tunnelstore"
)

const (
	credFileFlagAlias = "cred-file"
)

var (
	showDeletedFlag = &cli.BoolFlag{
		Name:    "show-deleted",
		Aliases: []string{"d"},
		Usage:   "Include deleted tunnels in the list",
	}
	listNameFlag = &cli.StringFlag{
		Name:    "name",
		Aliases: []string{"n"},
		Usage:   "List tunnels with the given `NAME`",
	}
	listExistedAtFlag = &cli.TimestampFlag{
		Name:        "when",
		Aliases:     []string{"w"},
		Usage:       "List tunnels that are active at the given `TIME` in RFC3339 format",
		Layout:      tunnelstore.TimeLayout,
		DefaultText: fmt.Sprintf("current time, %s", time.Now().Format(tunnelstore.TimeLayout)),
	}
	listIDFlag = &cli.StringFlag{
		Name:    "id",
		Aliases: []string{"i"},
		Usage:   "List tunnel by `ID`",
	}
	showRecentlyDisconnected = &cli.BoolFlag{
		Name:    "show-recently-disconnected",
		Aliases: []string{"rd"},
		Usage:   "Include connections that have recently disconnected in the list",
	}
	outputFormatFlag = &cli.StringFlag{
		Name:    "output",
		Aliases: []string{"o"},
		Usage:   "Render output using given `FORMAT`. Valid options are 'json' or 'yaml'",
	}
	forceFlag = &cli.BoolFlag{
		Name:    "force",
		Aliases: []string{"f"},
		Usage: "By default, if a tunnel is currently being run from a cloudflared, you can't " +
			"simultaneously rerun it again from a second cloudflared. The --force flag lets you " +
			"overwrite the previous tunnel. If you want to use a single hostname with multiple " +
			"tunnels, you can do so with Cloudflare's Load Balancer product.",
	}
	credentialsFileFlag = &cli.StringFlag{
		Name:    "credentials-file",
		Aliases: []string{credFileFlagAlias},
		Usage:   "File path of tunnel credentials",
	}
	forceDeleteFlag = &cli.BoolFlag{
		Name:    "force",
		Aliases: []string{"f"},
		Usage:   "Allows you to delete a tunnel, even if it has active connections.",
	}
)

func buildCreateCommand() *cli.Command {
	return &cli.Command{
		Name:      "create",
		Action:    cliutil.ErrorHandler(createCommand),
		Usage:     "Create a new tunnel with given name",
		ArgsUsage: "TUNNEL-NAME",
		Flags:     []cli.Flag{outputFormatFlag},
	}
}

// generateTunnelSecret as an array of 32 bytes using secure random number generator
func generateTunnelSecret() ([]byte, error) {
	randomBytes := make([]byte, 32)
	_, err := rand.Read(randomBytes)
	return randomBytes, err
}

func createCommand(c *cli.Context) error {
	sc, err := newSubcommandContext(c)
	if err != nil {
		return errors.Wrap(err, "error setting up logger")
	}

	if c.NArg() != 1 {
		return cliutil.UsageError(`"cloudflared tunnel create" requires exactly 1 argument, the name of tunnel to create.`)
	}
	name := c.Args().First()

	_, err = sc.create(name)
	return errors.Wrap(err, "failed to create tunnel")
}

func tunnelFilePath(tunnelID uuid.UUID, directory string) (string, error) {
	fileName := fmt.Sprintf("%v.json", tunnelID)
	filePath := filepath.Clean(fmt.Sprintf("%s/%s", directory, fileName))
	return homedir.Expand(filePath)
}

func writeTunnelCredentials(tunnelID uuid.UUID, accountID, originCertPath string, tunnelSecret []byte, logger logger.Service) error {
	originCertDir := filepath.Dir(originCertPath)
	filePath, err := tunnelFilePath(tunnelID, originCertDir)
	if err != nil {
		return err
	}
	body, err := json.Marshal(pogs.TunnelAuth{
		AccountTag:   accountID,
		TunnelSecret: tunnelSecret,
	})
	if err != nil {
		return errors.Wrap(err, "Unable to marshal tunnel credentials to JSON")
	}
	logger.Infof("Writing tunnel credentials to %v. cloudflared chose this file based on where your origin certificate was found.", filePath)
	logger.Infof("Keep this file secret. To revoke these credentials, delete the tunnel.")
	return ioutil.WriteFile(filePath, body, 400)
}

func validFilePath(path string) bool {
	fileStat, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !fileStat.IsDir()
}

func buildListCommand() *cli.Command {
	return &cli.Command{
		Name:      "list",
		Action:    cliutil.ErrorHandler(listCommand),
		Usage:     "List existing tunnels",
		ArgsUsage: " ",
		Flags:     []cli.Flag{outputFormatFlag, showDeletedFlag, listNameFlag, listExistedAtFlag, listIDFlag, showRecentlyDisconnected},
	}
}

func listCommand(c *cli.Context) error {
	sc, err := newSubcommandContext(c)
	if err != nil {
		return err
	}

	filter := tunnelstore.NewFilter()
	if !c.Bool("show-deleted") {
		filter.NoDeleted()
	}
	if name := c.String("name"); name != "" {
		filter.ByName(name)
	}
	if existedAt := c.Timestamp("time"); existedAt != nil {
		filter.ByExistedAt(*existedAt)
	}
	if id := c.String("id"); id != "" {
		tunnelID, err := uuid.Parse(id)
		if err != nil {
			return errors.Wrapf(err, "%s is not a valid tunnel ID", id)
		}
		filter.ByTunnelID(tunnelID)
	}

	tunnels, err := sc.list(filter)
	if err != nil {
		return err
	}

	if outputFormat := c.String(outputFormatFlag.Name); outputFormat != "" {
		return renderOutput(outputFormat, tunnels)
	}

	if len(tunnels) > 0 {
		fmtAndPrintTunnelList(tunnels, c.Bool("show-recently-disconnected"))
	} else {
		fmt.Println("You have no tunnels, use 'cloudflared tunnel create' to define a new tunnel")
	}
	return nil
}

func fmtAndPrintTunnelList(tunnels []*tunnelstore.Tunnel, showRecentlyDisconnected bool) {
	const (
		minWidth = 0
		tabWidth = 8
		padding  = 1
		padChar  = ' '
		flags    = 0
	)

	writer := tabwriter.NewWriter(os.Stdout, minWidth, tabWidth, padding, padChar, flags)
	defer writer.Flush()

	// Print column headers with tabbed columns
	fmt.Fprintln(writer, "ID\tNAME\tCREATED\tCONNECTIONS\t")

	// Loop through tunnels, create formatted string for each, and print using tabwriter
	for _, t := range tunnels {
		formattedStr := fmt.Sprintf(
			"%s\t%s\t%s\t%s\t",
			t.ID,
			t.Name,
			t.CreatedAt.Format(time.RFC3339),
			fmtConnections(t.Connections, showRecentlyDisconnected),
		)
		fmt.Fprintln(writer, formattedStr)
	}
}

func fmtConnections(connections []tunnelstore.Connection, showRecentlyDisconnected bool) string {

	// Count connections per colo
	numConnsPerColo := make(map[string]uint, len(connections))
	for _, connection := range connections {
		if !connection.IsPendingReconnect || showRecentlyDisconnected {
			numConnsPerColo[connection.ColoName]++
		}
	}

	// Get sorted list of colos
	sortedColos := []string{}
	for coloName := range numConnsPerColo {
		sortedColos = append(sortedColos, coloName)
	}
	sort.Strings(sortedColos)

	// Map each colo to its frequency, combine into output string.
	var output []string
	for _, coloName := range sortedColos {
		output = append(output, fmt.Sprintf("%dx%s", numConnsPerColo[coloName], coloName))
	}
	return strings.Join(output, ", ")
}

func buildDeleteCommand() *cli.Command {
	return &cli.Command{
		Name:      "delete",
		Action:    cliutil.ErrorHandler(deleteCommand),
		Usage:     "Delete existing tunnel by UUID or name",
		ArgsUsage: "TUNNEL",
		Flags:     []cli.Flag{credentialsFileFlag, forceDeleteFlag},
	}
}

func deleteCommand(c *cli.Context) error {
	sc, err := newSubcommandContext(c)
	if err != nil {
		return err
	}

	if c.NArg() < 1 {
		return cliutil.UsageError(`"cloudflared tunnel delete" requires at least 1 argument, the ID or name of the tunnel to delete.`)
	}

	tunnelIDs, err := sc.findIDs(c.Args().Slice())
	if err != nil {
		return err
	}

	return sc.delete(tunnelIDs)
}

func renderOutput(format string, v interface{}) error {
	switch format {
	case "json":
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(v)
	case "yaml":
		return yaml.NewEncoder(os.Stdout).Encode(v)
	default:
		return errors.Errorf("Unknown output format '%s'", format)
	}
}

func buildRunCommand() *cli.Command {
	flags := []cli.Flag{
		forceFlag,
		credentialsFileFlag,
		urlFlag(false),
		helloWorldFlag(false),
		createSocks5Flag(false),
	}
	flags = append(flags, sshFlags(false)...)
	var runCommandHelpTemplate = `NAME:
   {{.HelpName}} - {{.Usage}}

USAGE:
   {{.UsageText}}

DESCRIPTION:
   {{.Description}}

TUNNEL COMMAND OPTIONS:
	See cloudflared tunnel -h

RUN COMMAND OPTIONS:
   {{range .VisibleFlags}}{{.}}
   {{end}}
`
	return &cli.Command{
		Name:      "run",
		Action:    cliutil.ErrorHandler(runCommand),
		Usage:     "Proxy a local web server by running the given tunnel",
		UsageText: "cloudflared tunnel [tunnel command options] run [run command options]",
		ArgsUsage: "TUNNEL",
		Description: `Runs the tunnel identified by name or UUUD, creating a highly available connection 
   between your server and the Cloudflare edge.

   This command requires the tunnel credentials file created when "cloudflared tunnel create" was run, 
   however it does not need access to cert.pem from "cloudflared login". If you experience problems running
   the tunnel, "cloudflared tunnel cleanup" may help by removing any old connection records.

   All the flags from the tunnel command are available, note that they have to be specified before the run command. There are flags defined both in tunnel and run command. The one in run command will take precedence.
   For example cloudflared tunnel --url localhost:3000 run --url localhost:5000 <TUNNEL ID> will proxy requests to localhost:5000.
`,
		Flags:              flags,
		CustomHelpTemplate: runCommandHelpTemplate,
	}
}

func runCommand(c *cli.Context) error {
	sc, err := newSubcommandContext(c)
	if err != nil {
		return err
	}

	if c.NArg() != 1 {
		return cliutil.UsageError(`"cloudflared tunnel run" requires exactly 1 argument, the ID or name of the tunnel to run.`)
	}
	tunnelID, err := sc.findID(c.Args().First())
	if err != nil {
		return errors.Wrap(err, "error parsing tunnel ID")
	}

	return sc.run(tunnelID)
}

func buildCleanupCommand() *cli.Command {
	return &cli.Command{
		Name:      "cleanup",
		Action:    cliutil.ErrorHandler(cleanupCommand),
		Usage:     "Cleanup tunnel connections",
		ArgsUsage: "TUNNEL",
	}
}

func cleanupCommand(c *cli.Context) error {
	if c.NArg() < 1 {
		return cliutil.UsageError(`"cloudflared tunnel cleanup" requires at least 1 argument, the IDs of the tunnels to cleanup connections.`)
	}

	sc, err := newSubcommandContext(c)
	if err != nil {
		return err
	}

	tunnelIDs, err := sc.findIDs(c.Args().Slice())
	if err != nil {
		return err
	}

	return sc.cleanupConnections(tunnelIDs)
}

func buildRouteCommand() *cli.Command {
	return &cli.Command{
		Name:   "route",
		Action: cliutil.ErrorHandler(routeCommand),
		Usage:  "Define what hostname or load balancer can route to this tunnel",
		Description: `The route defines what hostname or load balancer will proxy requests to this tunnel.

   To route a hostname by creating a CNAME to tunnel's address:
      cloudflared tunnel route dns <tunnel ID> <hostname>
   To use this tunnel as a load balancer origin, creating pool and load balancer if necessary:
      cloudflared tunnel route lb <tunnel ID> <load balancer name> <load balancer pool>`,
		ArgsUsage: "dns|lb TUNNEL HOSTNAME [LB-POOL]",
	}
}

func dnsRouteFromArg(c *cli.Context) (tunnelstore.Route, error) {
	const (
		userHostnameIndex = 2
		expectedNArgs     = 3
	)
	if c.NArg() != expectedNArgs {
		return nil, cliutil.UsageError("Expected %d arguments, got %d", expectedNArgs, c.NArg())
	}
	userHostname := c.Args().Get(userHostnameIndex)
	if userHostname == "" {
		return nil, cliutil.UsageError("The third argument should be the hostname")
	} else if !validateHostname(userHostname) {
		return nil, errors.Errorf("%s is not a valid hostname", userHostname)
	}
	return tunnelstore.NewDNSRoute(userHostname), nil
}

func lbRouteFromArg(c *cli.Context) (tunnelstore.Route, error) {
	const (
		lbNameIndex   = 2
		lbPoolIndex   = 3
		expectedNArgs = 4
	)
	if c.NArg() != expectedNArgs {
		return nil, cliutil.UsageError("Expected %d arguments, got %d", expectedNArgs, c.NArg())
	}
	lbName := c.Args().Get(lbNameIndex)
	if lbName == "" {
		return nil, cliutil.UsageError("The third argument should be the load balancer name")
	} else if !validateHostname(lbName) {
		return nil, errors.Errorf("%s is not a valid load balancer name", lbName)
	}

	lbPool := c.Args().Get(lbPoolIndex)
	if lbPool == "" {
		return nil, cliutil.UsageError("The fourth argument should be the pool name")
	} else if !validateName(lbPool) {
		return nil, errors.Errorf("%s is not a valid pool name", lbPool)
	}

	return tunnelstore.NewLBRoute(lbName, lbPool), nil
}

var nameRegex = regexp.MustCompile("^[_a-zA-Z0-9][-_.a-zA-Z0-9]*$")

func validateName(s string) bool {
	return nameRegex.MatchString(s)
}

func validateHostname(s string) bool {
	// Slightly stricter than PunyCodeProfile
	idnaProfile := idna.New(
		idna.ValidateLabels(true),
		idna.VerifyDNSLength(true))

	puny, err := idnaProfile.ToASCII(s)
	return err == nil && validateName(puny)
}

func routeCommand(c *cli.Context) error {
	if c.NArg() < 2 {
		return cliutil.UsageError(`"cloudflared tunnel route" requires the first argument to be the route type(dns or lb), followed by the ID or name of the tunnel`)
	}
	sc, err := newSubcommandContext(c)
	if err != nil {
		return err
	}

	const tunnelIDIndex = 1

	routeType := c.Args().First()
	var route tunnelstore.Route
	var tunnelID uuid.UUID
	switch routeType {
	case "dns":
		tunnelID, err = sc.findID(c.Args().Get(tunnelIDIndex))
		if err != nil {
			return err
		}
		route, err = dnsRouteFromArg(c)
		if err != nil {
			return err
		}
	case "lb":
		tunnelID, err = sc.findID(c.Args().Get(tunnelIDIndex))
		if err != nil {
			return err
		}
		route, err = lbRouteFromArg(c)
		if err != nil {
			return err
		}
	default:
		return cliutil.UsageError("%s is not a recognized route type. Supported route types are dns and lb", routeType)
	}

	res, err := sc.route(tunnelID, route)
	if err != nil {
		return err
	}

	sc.logger.Infof(res.SuccessSummary())
	return nil
}
