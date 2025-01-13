package cmd

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudforet-io/cfctl/cmd/commands"
	pkggrpc "github.com/cloudforet-io/cfctl/pkg/grpc"
	"github.com/cloudforet-io/cfctl/pkg/rest"
	"gopkg.in/yaml.v3"

	"github.com/spf13/viper"

	"github.com/cloudforet-io/cfctl/cmd/other"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
)

var cachedEndpointsMap map[string]string

// Config represents the configuration structure
type Config struct {
	Environment string
	Endpoint    string
	Token       string
}

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "cfctl",
	Short: "cfctl controls the SpaceONE services.",
	Long: `cfctl controls the SpaceONE services.
  Find more information at: 
    - https://github.com/cloudforet-io/cfctl
    - https://docs.spaceone.megazone.io/developers/setup/cfctl (English)
    - https://docs.spaceone.megazone.io/ko/developers/setup/cfctl (Korean)`,
	// Uncomment the following line if your bare application
	// has an action associated with it:
	// Run: func(cmd *cobra.Command, args []string) { },
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	args := os.Args[1:]

	if len(args) > 1 {
		// Check if the first argument is a service name and second is a short name
		v := viper.New()
		if home, err := os.UserHomeDir(); err == nil {
			settingPath := filepath.Join(home, ".cfctl", "setting.yaml")
			v.SetConfigFile(settingPath)
			v.SetConfigType("yaml")

			if err := v.ReadInConfig(); err == nil {
				serviceName := args[0]
				shortName := args[1]
				if command := v.GetString(fmt.Sprintf("short_names.%s.%s", serviceName, shortName)); command != "" {
					// Replace the short name with the actual command
					newArgs := append([]string{args[0]}, strings.Fields(command)...)
					newArgs = append(newArgs, args[2:]...)
					os.Args = append([]string{os.Args[0]}, newArgs...)
				}
			}
		}
	}

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	// Initialize available commands group
	AvailableCommands := &cobra.Group{
		ID:    "available",
		Title: "Available Commands:",
	}
	rootCmd.AddGroup(AvailableCommands)

	done := make(chan bool)
	go func() {
		if endpoints, err := loadCachedEndpoints(); err == nil {
			cachedEndpointsMap = endpoints
		}
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(50 * time.Millisecond):
		_, err := fmt.Fprintf(os.Stderr, "Warning: Cache loading timed out\n")
		if err != nil {
			return
		}
	}

	if len(os.Args) > 1 && (os.Args[1] == "__complete" || os.Args[1] == "completion") {
		pterm.DisableColor()
	}

	// Determine if the current command is 'setting environment -l'
	skipDynamicCommands := false
	if len(os.Args) >= 2 && os.Args[1] == "setting" {
		// Skip dynamic commands for all setting related operations
		skipDynamicCommands = true
	}

	if !skipDynamicCommands {
		if err := addDynamicServiceCommands(); err != nil {
			showInitializationGuide()
		}
	}

	// Initialize other commands group
	OtherCommands := &cobra.Group{
		ID:    "other",
		Title: "Other Commands:",
	}
	rootCmd.AddGroup(OtherCommands)
	rootCmd.AddCommand(other.ApiResourcesCmd)
	rootCmd.AddCommand(other.SettingCmd)
	rootCmd.AddCommand(other.LoginCmd)
	rootCmd.AddCommand(other.ShortNameCmd)

	// Set default group for commands without a group
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() != "help" && cmd.Name() != "completion" && cmd.GroupID == "" {
			cmd.GroupID = "other"
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Unable to find home directory: %v", err)
	}
	viper.AddConfigPath(filepath.Join(home, ".cfctl"))
	viper.SetConfigName("setting")
	viper.SetConfigType("yaml")
}

// showInitializationGuide displays a helpful message when configuration is missing
func showInitializationGuide() {
	// Skip showing guide for certain commands
	if len(os.Args) >= 2 && (os.Args[1] == "setting" ||
		os.Args[1] == "login" ||
		os.Args[1] == "api-resources" ||
		os.Args[1] == "completion") {
		return
	}

	// Get current environment from setting file
	home, err := os.UserHomeDir()
	if err != nil {
		pterm.Error.Printf("Unable to find home directory: %v\n", err)
		return
	}

	settingFile := filepath.Join(home, ".cfctl", "setting.yaml")
	mainV := viper.New()
	mainV.SetConfigFile(settingFile)
	mainV.SetConfigType("yaml")

	if err := mainV.ReadInConfig(); err != nil {
		pterm.Warning.Printf("No valid configuration found.\n")
		pterm.Info.Println("Please run 'cfctl setting init' to set up your configuration.")
		return
	}

	currentEnv := mainV.GetString("environment")
	if currentEnv == "" {
		pterm.Warning.Printf("No environment selected.\n")
		pterm.Info.Println("Please run 'cfctl setting init' to set up your configuration.")
		return
	}

	// Check if current environment is app type and token is empty
	if strings.HasSuffix(currentEnv, "-app") {
		envConfig := mainV.Sub(fmt.Sprintf("environments.%s", currentEnv))
		if envConfig == nil || envConfig.GetString("token") == "" {
			// Get URL from environment config
			url := envConfig.GetString("url")
			if url == "" {
				// Fallback URL if not specified in config
				parts := strings.Split(currentEnv, "-")
				if len(parts) >= 2 {
					serviceName := parts[0] // cloudone, spaceone, etc.
					url = fmt.Sprintf("https://%s.console.dev.spaceone.dev", serviceName)
				} else {
					url = "https://console.spaceone.dev"
				}
			}

			pterm.DefaultBox.
				WithTitle("Token Not Found").
				WithTitleTopCenter().
				WithBoxStyle(pterm.NewStyle(pterm.FgWhite)).
				WithRightPadding(1).
				WithLeftPadding(1).
				WithTopPadding(0).
				WithBottomPadding(0).
				Println("Please follow the instructions below to obtain an App Token.")

			boxContent := fmt.Sprintf(`Please follow these steps to obtain an App Token:

1. Visit %s
2. Go to Admin page or Workspace page
3. Navigate to the App page
4. Click [Create] button
5. Copy the generated App Token
6. Update your settings:
     Path: %s
     Environment: %s
     Field: "token"`,
				pterm.FgLightCyan.Sprint(url),
				pterm.FgLightYellow.Sprint(settingFile),
				pterm.FgLightGreen.Sprint(currentEnv))

			pterm.DefaultBox.
				WithTitle("Setup Instructions").
				WithTitleTopCenter().
				WithBoxStyle(pterm.NewStyle(pterm.FgLightBlue)).
				Println(boxContent)

			pterm.Info.Println("After updating the token, please try your command again.")
		}
	} else if strings.HasSuffix(currentEnv, "-user") {
		// Get endpoint from environment config
		envConfig := mainV.Sub(fmt.Sprintf("environments.%s", currentEnv))
		if envConfig == nil {
			pterm.Warning.Printf("No environment configuration found.\n")
			return
		}

		endpoint := envConfig.GetString("endpoint")

		// Skip authentication warning for gRPC+SSL endpoints
		if strings.HasPrefix(endpoint, "grpc+ssl://") {
			return
		}

		pterm.Warning.Printf("Authentication required.\n")
		pterm.Info.Println("To see Available Commands, please authenticate first:")
		pterm.Info.Println("$ cfctl login")
	}
}

func addDynamicServiceCommands() error {
	config, err := loadConfig()
	if err != nil {
		return err
	}

	// For non-local environments
	endpoint := config.Endpoint
	var apiEndpoint string

	if strings.HasPrefix(endpoint, "grpc+ssl://") {
		apiEndpoint = endpoint
	} else if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		apiEndpoint, err = rest.GetAPIEndpoint(endpoint)
		if err != nil {
			return fmt.Errorf("failed to get API endpoint: %v", err)
		}
	}

	// Try to use cached endpoints first
	if cachedEndpointsMap != nil {
		currentService := ""
		if strings.HasPrefix(endpoint, "grpc+ssl://") {
			parts := strings.Split(endpoint, "://")
			if len(parts) == 2 {
				hostParts := strings.Split(parts[1], ".")
				if len(hostParts) > 0 {
					currentService = hostParts[0]
				}
			}
		}

		if currentService != "identity" && currentService != "" {
			if cmd := createServiceCommand(currentService); cmd != nil {
				cmd.GroupID = "available"
				rootCmd.AddCommand(cmd)
			}
			return nil
		}

		// If identity service or no specific service, add all available commands
		for serviceName := range cachedEndpointsMap {
			cmd := createServiceCommand(serviceName)
			cmd.GroupID = "available"
			rootCmd.AddCommand(cmd)
		}
		return nil
	}

	// If no cached endpoints, show progress with detailed messages
	progressbar, _ := pterm.DefaultProgressbar.
		WithTotal(4).
		WithTitle(fmt.Sprintf("Setting up %s environment", config.Environment)).
		Start()

	progressbar.UpdateTitle("Fetching available service endpoints from the API server")
	endpointsMap, err := rest.FetchEndpointsMap(apiEndpoint)
	if err != nil {
		return fmt.Errorf("failed to fetch services: %v", err)
	}
	progressbar.Increment()

	progressbar.UpdateTitle(fmt.Sprintf("Caching endpoints to %s/.cfctl/cache for faster access", os.Getenv("HOME")))
	cachedEndpointsMap = endpointsMap
	if err := saveEndpointsCache(endpointsMap); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Failed to cache endpoints: %v\n", err)
	}
	progressbar.Increment()

	progressbar.UpdateTitle("Registering available service commands")
	// Add commands based on the current service
	currentService := ""
	if strings.HasPrefix(endpoint, "grpc+ssl://") {
		parts := strings.Split(endpoint, "://")
		if len(parts) == 2 {
			hostParts := strings.Split(parts[1], ".")
			if len(hostParts) > 0 {
				currentService = hostParts[0]
			}
		}
	}

	if currentService != "identity" && currentService != "" {
		if cmd := createServiceCommand(currentService); cmd != nil {
			cmd.GroupID = "available"
			rootCmd.AddCommand(cmd)
		}
	} else {
		for serviceName := range endpointsMap {
			cmd := createServiceCommand(serviceName)
			cmd.GroupID = "available"
			rootCmd.AddCommand(cmd)
		}
	}
	progressbar.Increment()

	progressbar.UpdateTitle("Setup completed successfully!")
	progressbar.Increment()

	fmt.Println() // Add newline after progress bar
	return nil
}

func loadCachedEndpoints() (map[string]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	settingFile := filepath.Join(home, ".cfctl", "setting.yaml")
	settingData, err := os.ReadFile(settingFile)
	if err != nil {
		return nil, err
	}

	var settings struct {
		Environment string `yaml:"environment"`
	}

	if err := yaml.Unmarshal(settingData, &settings); err != nil {
		return nil, err
	}

	if settings.Environment == "" {
		return nil, fmt.Errorf("no environment set")
	}

	cacheFile := filepath.Join(home, ".cfctl", "cache", settings.Environment, "endpoints.yaml")
	data, err := os.ReadFile(cacheFile)
	if err != nil {
		return nil, err
	}

	cacheInfo, err := os.Stat(cacheFile)
	if err != nil {
		return nil, err
	}

	if time.Since(cacheInfo.ModTime()) > 24*time.Hour {
		return nil, fmt.Errorf("cache expired")
	}

	var endpoints map[string]string
	if err := yaml.Unmarshal(data, &endpoints); err != nil {
		return nil, err
	}

	return endpoints, nil
}

func saveEndpointsCache(endpoints map[string]string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	// Get current environment from main setting file
	mainV := viper.New()
	mainV.SetConfigFile(filepath.Join(home, ".cfctl", "setting.yaml"))
	mainV.SetConfigType("yaml")
	if err := mainV.ReadInConfig(); err != nil {
		return err
	}

	currentEnv := mainV.GetString("environment")
	if currentEnv == "" {
		return fmt.Errorf("no environment set")
	}

	// Create environment-specific cache directory
	envCacheDir := filepath.Join(home, ".cfctl", "cache", currentEnv)
	if err := os.MkdirAll(envCacheDir, 0755); err != nil {
		return err
	}

	data, err := yaml.Marshal(endpoints)
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(envCacheDir, "endpoints.yaml"), data, 0644)
}

// loadConfig loads configuration from both main and cache setting files
func loadConfig() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("unable to find home directory: %v", err)
	}

	settingFile := filepath.Join(home, ".cfctl", "setting.yaml")

	// Read main setting file
	mainV := viper.New()
	mainV.SetConfigFile(settingFile)
	mainV.SetConfigType("yaml")
	if err := mainV.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read setting file")
	}

	currentEnv := mainV.GetString("environment")
	if currentEnv == "" {
		return nil, fmt.Errorf("no environment set")
	}

	// Get environment config
	envConfig := mainV.Sub(fmt.Sprintf("environments.%s", currentEnv))
	if envConfig == nil {
		return nil, fmt.Errorf("environment %s not found", currentEnv)
	}

	endpoint := envConfig.GetString("endpoint")
	if endpoint == "" {
		return nil, fmt.Errorf("no endpoint found in configuration")
	}

	config := &Config{
		Environment: currentEnv,
		Endpoint:    endpoint,
	}

	if strings.HasSuffix(currentEnv, "-app") {
		config.Token = envConfig.GetString("token")
	}

	return config, nil
}

func createServiceCommand(serviceName string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     serviceName + " [verb] [resource] [flags]",
		Short:   fmt.Sprintf("Interact with the %s service", serviceName),
		Long:    fmt.Sprintf("Use this command to interact with the %s service.", serviceName),
		GroupID: "available",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}

			verb := args[0]
			resource := ""
			if len(args) > 1 {
				resource = args[1]
			}

			if verb == "api_resources" {
				return commands.ListAPIResources(serviceName)
			}

			parameters, _ := cmd.Flags().GetStringArray("parameter")
			jsonParameter, _ := cmd.Flags().GetString("json-parameter")
			fileParameter, _ := cmd.Flags().GetString("file-parameter")
			outputFormat, _ := cmd.Flags().GetString("output")
			copyToClipboard, _ := cmd.Flags().GetBool("copy")

			options := &pkggrpc.FetchOptions{
				Parameters:      parameters,
				JSONParameter:   jsonParameter,
				FileParameter:   fileParameter,
				OutputFormat:    outputFormat,
				CopyToClipboard: copyToClipboard,
			}

			_, err := pkggrpc.FetchService(serviceName, verb, resource, options)
			if err != nil {
				pterm.Error.Println(err.Error())
				return nil
			}
			return nil
		},
		Example: fmt.Sprintf(`  # List available API resources
  cfctl %[1]s api_resources

  # List resources
  cfctl %[1]s list User
  
  # Create a resource
  cfctl %[1]s create Project -p name=test`, serviceName),
	}

	cmd.Flags().StringArrayP("parameter", "p", []string{}, "Input Parameter (-p <key>=<value> -p ...)")
	cmd.Flags().StringP("json-parameter", "j", "", "JSON type parameter")
	cmd.Flags().StringP("file-parameter", "f", "", "YAML file parameter")
	cmd.Flags().StringP("output", "o", "yaml", "Output format (yaml, json, table, csv)")
	cmd.Flags().BoolP("copy", "y", false, "Copy the output to the clipboard")

	return cmd
}
