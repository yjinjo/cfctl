package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/cloudforet-io/cfctl/pkg/configs"
	"github.com/cloudforet-io/cfctl/pkg/format"
	"github.com/eiannone/keyboard"
	"github.com/pterm/pterm"
	"github.com/spf13/viper"

	"google.golang.org/grpc/metadata"

	"github.com/jhump/protoreflect/dynamic"
	"github.com/jhump/protoreflect/grpcreflect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection/grpc_reflection_v1alpha"

	"gopkg.in/yaml.v3"
)

type Environment struct {
	Endpoint string `yaml:"endpoint"`
	Proxy    string `yaml:"proxy"`
	Token    string `yaml:"token"`
}

type Config struct {
	Environment  string                 `yaml:"environment"`
	Environments map[string]Environment `yaml:"environments"`
}

// FetchOptions holds the flag values for a command
type FetchOptions struct {
	Parameters           []string
	JSONParameter        string
	FileParameter        string
	APIVersion           string
	OutputFormat         string
	OutputFormatExplicit bool
	CopyToClipboard      bool
	SortBy               string
	MinimalColumns       bool
	Columns              string
	Rows                 int
	Page                 int
	PageSize             int
	NoPaging             bool
}

// FetchService handles the execution of gRPC commands for all services
func FetchService(serviceName string, verb string, resourceName string, options *FetchOptions) (map[string]interface{}, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get user home directory: %v", err)
	}

	// Read configuration file
	mainViper := viper.New()
	mainViper.SetConfigFile(filepath.Join(homeDir, ".cfctl", "setting.yaml"))
	mainViper.SetConfigType("yaml")
	if err := mainViper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read configuration file. Please run 'cfctl login' first")
	}

	// Check current environment
	currentEnv := mainViper.GetString("environment")
	if currentEnv == "" {
		return nil, fmt.Errorf("no environment set. Please run 'cfctl login' first")
	}

	// Load configuration first
	config, err := loadConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %v", err)
	}

	token := config.Environments[config.Environment].Token
	if token == "" {
		pterm.Error.Println("No token found for authentication.")

		// Get current endpoint
		endpoint := config.Environments[config.Environment].Endpoint

		if config.Environment == "local" {
			// Local environment message
			pterm.Info.Printf("Using endpoint: %s\n", endpoint)
			return nil, nil
		} else if strings.HasSuffix(config.Environment, "-app") {
			// App environment message
			headerBox := pterm.DefaultBox.WithTitle("App Guide").
				WithTitleTopCenter().
				WithRightPadding(4).
				WithLeftPadding(4).
				WithBoxStyle(pterm.NewStyle(pterm.FgLightCyan))

			appTokenExplain := "Please create a Domain Admin App in SpaceONE Console.\n" +
				"This requires Domain Admin privilege.\n\n" +
				"Or Please create a Workspace App in SpaceONE Console.\n" +
				"This requires Workspace Owner privilege."

			pterm.Info.Printf("Using endpoint: %s\n", endpoint)
			headerBox.Println(appTokenExplain)
			fmt.Println()

			steps := []string{
				"1. Go to SpaceONE Console",
				"2. Navigate to either 'Admin > App Page' or specific 'Workspace > App page'",
				"3. Click 'Create' to create your App",
				"4. Copy value of either 'client_secret' from Client ID or 'token' from Spacectl (CLI)",
			}

			yamlExample := pterm.DefaultBox.WithTitle("Config Example").
				WithTitleTopCenter().
				WithRightPadding(4).
				WithLeftPadding(4).
				Sprint(fmt.Sprintf("environment: %s\nenvironments:\n    %s:\n        endpoint: %s\n        proxy: true\n        token: %s",
					currentEnv,
					currentEnv,
					endpoint,
					pterm.FgLightCyan.Sprint("YOUR_COPIED_TOKEN")))

			instructionBox := pterm.DefaultBox.WithTitle("Required Steps").
				WithTitleTopCenter().
				WithRightPadding(4).
				WithLeftPadding(4)

			allSteps := append(steps,
				fmt.Sprintf("5. Add the token under the proxy in your config file:\n%s", yamlExample),
				"6. Run 'cfctl login' again")

			instructionBox.Println(strings.Join(allSteps, "\n\n"))

		} else if strings.HasSuffix(config.Environment, "-user") {
			// User environment message
			headerBox := pterm.DefaultBox.WithTitle("Authentication Required").
				WithTitleTopCenter().
				WithRightPadding(4).
				WithLeftPadding(4).
				WithBoxStyle(pterm.NewStyle(pterm.FgLightCyan))

			authExplain := "Please login to SpaceONE Console first.\n" +
				"This requires your SpaceONE credentials."

			headerBox.Println(authExplain)
			fmt.Println()

			steps := []string{
				"1. Run 'cfctl login'",
				"2. Enter your credentials when prompted",
				"3. Select your scope",
				"4. Try your command again",
			}

			instructionBox := pterm.DefaultBox.WithTitle("Required Steps").
				WithTitleTopCenter().
				WithRightPadding(4).
				WithLeftPadding(4)

			instructionBox.Println(strings.Join(steps, "\n\n"))
		}

		return nil, nil
	}

	// Get hostPort based on environment prefix
	var hostPort string
	var apiEndpoint string
	var identityEndpoint string
	var hasIdentityService bool
	if strings.HasPrefix(config.Environments[config.Environment].Endpoint, "grpc://") {
		hostPort = strings.TrimPrefix(config.Environments[config.Environment].Endpoint, "grpc://")
	} else {
		apiEndpoint, err = configs.GetAPIEndpoint(config.Environments[config.Environment].Endpoint)
		if err != nil {
			pterm.Error.Printf("Failed to get API endpoint: %v\n", err)
			os.Exit(1)
		}
		// Get identity service endpoint
		identityEndpoint, hasIdentityService, err = configs.GetIdentityEndpoint(apiEndpoint)
		if err != nil {
			pterm.Error.Printf("Failed to get identity endpoint: %v\n", err)
			os.Exit(1)
		}

		if !hasIdentityService {
			urlParts := strings.Split(apiEndpoint, "//")
			if len(urlParts) != 2 {
				return nil, fmt.Errorf("invalid API endpoint format: %s", apiEndpoint)
			}

			domainParts := strings.Split(urlParts[1], ".")
			if len(domainParts) > 0 {
				port := extractPortFromParts(domainParts)
				if strings.Contains(domainParts[len(domainParts)-1], ":") {
					parts := strings.Split(domainParts[len(domainParts)-1], ":")
					domainParts[len(domainParts)-1] = parts[0]
				}

				domainParts[0] = format.ConvertServiceName(serviceName)
				hostPort = strings.Join(domainParts, ".") + port
			}
		} else {
			trimmedEndpoint := strings.TrimPrefix(identityEndpoint, "grpc+ssl://")
			parts := strings.Split(trimmedEndpoint, ".")
			if len(parts) < 4 {
				return nil, fmt.Errorf("invalid endpoint format: %s", trimmedEndpoint)
			}

			// Replace 'identity' with the converted service name
			parts[0] = format.ConvertServiceName(serviceName)
			hostPort = strings.Join(parts, ".")
		}
	}

	// Configure gRPC connection
	var conn *grpc.ClientConn
	if strings.HasPrefix(config.Environments[config.Environment].Endpoint, "grpc://") {
		hostPort := strings.TrimPrefix(config.Environments[config.Environment].Endpoint, "grpc://")
		// For local environment, use insecure connection
		conn, err = grpc.Dial(hostPort, grpc.WithInsecure())
		if err != nil {
			pterm.Error.Printf("Cannot connect to local gRPC server (%s)\n", hostPort)
			pterm.Info.Println("Please check if your gRPC server is running")
			return nil, fmt.Errorf("failed to connect to local server: %v", err)
		}
	} else {
		// Existing SSL connection logic for non-local environments
		tlsConfig := &tls.Config{
			InsecureSkipVerify: false,
		}
		creds := credentials.NewTLS(tlsConfig)
		conn, err = grpc.Dial(hostPort, grpc.WithTransportCredentials(creds))
		if err != nil {
			return nil, fmt.Errorf("connection failed: %v", err)
		}
	}
	defer conn.Close()

	// Create reflection client for both service calls and minimal fields detection
	ctx := metadata.AppendToOutgoingContext(context.Background(), "token", config.Environments[config.Environment].Token)
	refClient := grpcreflect.NewClient(ctx, grpc_reflection_v1alpha.NewServerReflectionClient(conn))
	defer refClient.Reset()

	// Check for alias
	aliases, err := configs.ListAliases()
	if err != nil {
		return nil, fmt.Errorf("failed to load aliases: %v", err)
	}

	// Check if the verb is an alias
	if serviceAliases, ok := aliases[serviceName].(map[string]interface{}); ok {
		if cmd, ok := serviceAliases[verb].(string); ok {
			// Split the alias command
			parts := strings.Fields(cmd)
			if len(parts) >= 2 {
				verb = parts[0]
				resourceName = parts[1]

				// If the command from alias is 'list'
				if verb == "list" {
					if !options.OutputFormatExplicit {
						options.OutputFormat = "table"
					}

					// Create new options for list command
					newOptions := &FetchOptions{
						Parameters:           options.Parameters,
						JSONParameter:        options.JSONParameter,
						FileParameter:        options.FileParameter,
						APIVersion:           options.APIVersion,
						OutputFormat:         options.OutputFormat,
						OutputFormatExplicit: options.OutputFormatExplicit,
						CopyToClipboard:      options.CopyToClipboard,
						MinimalColumns:       false, // Always show all columns for alias
						PageSize:             15,    // Default page size
					}

					options = newOptions
				}
			}
		}
	}

	// Call the service
	jsonBytes, err := fetchJSONResponse(config, serviceName, verb, resourceName, options, apiEndpoint, identityEndpoint, hasIdentityService)
	if err != nil {
		// Check if the error is about missing required parameters
		if strings.Contains(err.Error(), "ERROR_REQUIRED_PARAMETER") {
			// Extract parameter name from error message
			paramName := extractParameterName(err.Error())
			if paramName != "" {
				return nil, fmt.Errorf("missing required parameter: %s", paramName)
			}
		}
		return nil, err
	}

	// Unmarshal JSON bytes to a map
	var respMap map[string]interface{}
	if err = json.Unmarshal(jsonBytes, &respMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON: %v", err)
	}

	// Print the data if not in watch mode
	if options.OutputFormat != "" {
		if options.SortBy != "" && verb == "list" {
			if results, ok := respMap["results"].([]interface{}); ok {
				// Sort the results by the specified field
				sort.Slice(results, func(i, j int) bool {
					iMap := results[i].(map[string]interface{})
					jMap := results[j].(map[string]interface{})

					iVal, iOk := iMap[options.SortBy]
					jVal, jOk := jMap[options.SortBy]

					// Handle cases where the field doesn't exist
					if !iOk && !jOk {
						return false
					} else if !iOk {
						return false
					} else if !jOk {
						return true
					}

					// Compare based on type
					switch v := iVal.(type) {
					case string:
						return v < jVal.(string)
					case float64:
						return v < jVal.(float64)
					case bool:
						return v && !jVal.(bool)
					default:
						return false
					}
				})
				respMap["results"] = results
			}
		}

		if options.Rows > 0 && verb == "list" {
			if results, ok := respMap["results"].([]interface{}); ok {
				if len(results) > options.Rows {
					respMap["results"] = results[:options.Rows]
				}
			}
		}

		// Filter columns if specified
		if options.Columns != "" && verb == "list" {
			if results, ok := respMap["results"].([]interface{}); ok {
				columns := strings.Split(options.Columns, ",")
				filteredResults := make([]interface{}, len(results))

				for i, result := range results {
					if resultMap, ok := result.(map[string]interface{}); ok {
						filteredMap := make(map[string]interface{})
						for _, col := range columns {
							if val, exists := resultMap[strings.TrimSpace(col)]; exists {
								filteredMap[strings.TrimSpace(col)] = val
							}
						}
						filteredResults[i] = filteredMap
					}
				}
				respMap["results"] = filteredResults
			}
		}

		printData(respMap, options, serviceName, verb, resourceName, refClient)
	}

	return respMap, nil
}

// extractParameterName extracts the parameter name from the error message
func extractParameterName(errMsg string) string {
	if strings.Contains(errMsg, "Required parameter. (key = ") {
		start := strings.Index(errMsg, "key = ") + 6
		end := strings.Index(errMsg[start:], ")")
		if end != -1 {
			return errMsg[start : start+end]
		}
	}
	return ""
}

func extractPortFromParts(parts []string) string {
	if len(parts) == 0 {
		return ":443"
	}

	lastPart := parts[len(parts)-1]
	if strings.Contains(lastPart, ":") {
		portParts := strings.Split(lastPart, ":")
		if len(portParts) == 2 {
			return ":" + portParts[1]
		}
	}

	return ":443"
}

// promptForParameter prompts the user to enter a value for the given parameter
func promptForParameter(paramName string) (string, error) {
	prompt := fmt.Sprintf("Please enter value for '%s'", paramName)
	result, err := pterm.DefaultInteractiveTextInput.WithDefaultText("").Show(prompt)
	if err != nil {
		return "", fmt.Errorf("failed to read input: %v", err)
	}
	return result, nil
}

func loadConfig() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %v", err)
	}

	// Load main configuration file
	mainV := viper.New()
	mainConfigPath := filepath.Join(home, ".cfctl", "setting.yaml")
	mainV.SetConfigFile(mainConfigPath)
	mainV.SetConfigType("yaml")
	if err := mainV.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	currentEnv := mainV.GetString("environment")
	if currentEnv == "" {
		return nil, fmt.Errorf("no environment set in config")
	}

	// Get environment config from main config file
	envConfig := &Environment{
		Endpoint: mainV.GetString(fmt.Sprintf("environments.%s.endpoint", currentEnv)),
		Proxy:    mainV.GetString(fmt.Sprintf("environments.%s.proxy", currentEnv)),
		Token:    mainV.GetString(fmt.Sprintf("environments.%s.token", currentEnv)),
	}

	// Handle token based on environment type
	if strings.HasSuffix(currentEnv, "-user") {
		// For user environments, read from access_token file (Actual token is grant_token)
		grantTokenPath := filepath.Join(home, ".cfctl", "cache", currentEnv, "access_token")
		tokenBytes, err := os.ReadFile(grantTokenPath)
		if err == nil {
			envConfig.Token = strings.TrimSpace(string(tokenBytes))
		}
	} else if strings.HasSuffix(currentEnv, "-app") {
		// For app environments, get token from main config
		envConfig.Token = mainV.GetString(fmt.Sprintf("environments.%s.token", currentEnv))
	} else if currentEnv == "local" {
		// For local environment, get token from main config
		envConfig.Token = mainV.GetString(fmt.Sprintf("environments.%s.token", currentEnv))
	}

	if envConfig == nil {
		return nil, fmt.Errorf("environment '%s' not found in config files", currentEnv)
	}

	return &Config{
		Environment: currentEnv,
		Environments: map[string]Environment{
			currentEnv: *envConfig,
		},
	}, nil
}

func fetchJSONResponse(config *Config, serviceName string, verb string, resourceName string, options *FetchOptions, apiEndpoint, identityEndpoint string, hasIdentityService bool) ([]byte, error) {
	var conn *grpc.ClientConn
	var err error
	var hostPort string

	if verb == "list" && options.Page > 0 {
		options.Parameters = append(options.Parameters,
			fmt.Sprintf("page=%d", options.Page),
			fmt.Sprintf("page_size=%d", options.PageSize))
	}

	if strings.HasPrefix(config.Environments[config.Environment].Endpoint, "grpc://") {
		hostPort = strings.TrimPrefix(config.Environments[config.Environment].Endpoint, "grpc://")
		conn, err = grpc.Dial(hostPort, grpc.WithInsecure(),
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(10*1024*1024),
				grpc.MaxCallSendMsgSize(10*1024*1024),
			))
		if err != nil {
			return nil, fmt.Errorf("connection failed: unable to connect to local server: %v", err)
		}
	} else {
		if !hasIdentityService {
			// Handle gRPC+SSL protocol directly
			if strings.HasPrefix(config.Environments[config.Environment].Endpoint, "grpc+ssl://") {
				endpoint := config.Environments[config.Environment].Endpoint
				parts := strings.Split(endpoint, "/")
				endpoint = strings.Join(parts[:len(parts)-1], "/")
				parts = strings.Split(endpoint, "://")
				if len(parts) != 2 {
					return nil, fmt.Errorf("invalid endpoint format: %s", endpoint)
				}

				hostParts := strings.Split(parts[1], ".")
				if len(hostParts) < 4 {
					return nil, fmt.Errorf("invalid endpoint format: %s", endpoint)
				}

				// Replace service name
				hostParts[0] = format.ConvertServiceName(serviceName)
				hostPort = strings.Join(hostParts, ".")
			} else {
				// Original HTTP/HTTPS handling
				urlParts := strings.Split(apiEndpoint, "//")
				if len(urlParts) != 2 {
					return nil, fmt.Errorf("invalid API endpoint format: %s", apiEndpoint)
				}

				domainParts := strings.Split(urlParts[1], ".")
				if len(domainParts) > 0 {
					port := extractPortFromParts(domainParts)
					if strings.Contains(domainParts[len(domainParts)-1], ":") {
						parts := strings.Split(domainParts[len(domainParts)-1], ":")
						domainParts[len(domainParts)-1] = parts[0]
					}

					domainParts[0] = format.ConvertServiceName(serviceName)
					hostPort = strings.Join(domainParts, ".") + port
				}
			}
		} else {
			trimmedEndpoint := strings.TrimPrefix(identityEndpoint, "grpc+ssl://")
			parts := strings.Split(trimmedEndpoint, ".")
			if len(parts) < 4 {
				return nil, fmt.Errorf("invalid endpoint format: %s", trimmedEndpoint)
			}

			// Replace 'identity' with the converted service name
			parts[0] = format.ConvertServiceName(serviceName)
			hostPort = strings.Join(parts, ".")
		}

		tlsConfig := &tls.Config{
			InsecureSkipVerify: false,
		}
		creds := credentials.NewTLS(tlsConfig)

		conn, err = grpc.Dial(hostPort,
			grpc.WithTransportCredentials(creds),
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(10*1024*1024),
				grpc.MaxCallSendMsgSize(10*1024*1024),
			))
		if err != nil {
			return nil, fmt.Errorf("connection failed: unable to connect to %s: %v", hostPort, err)
		}
	}

	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {

		}
	}(conn)

	ctx := metadata.AppendToOutgoingContext(context.Background(), "token", config.Environments[config.Environment].Token)
	refClient := grpcreflect.NewClient(ctx, grpc_reflection_v1alpha.NewServerReflectionClient(conn))
	defer refClient.Reset()

	fullServiceName, err := discoverService(refClient, serviceName, resourceName)
	if err != nil {
		return nil, fmt.Errorf("failed to discover service: %v", err)
	}

	serviceDesc, err := refClient.ResolveService(fullServiceName)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve service %s: %v", fullServiceName, err)
	}

	methodDesc := serviceDesc.FindMethodByName(verb)
	if methodDesc == nil {
		return nil, fmt.Errorf("method not found: %s", verb)
	}

	// Create request and response messages
	reqMsg := dynamic.NewMessage(methodDesc.GetInputType())
	respMsg := dynamic.NewMessage(methodDesc.GetOutputType())

	// Parse and set input parameters
	inputParams, err := parseParameters(options)
	if err != nil {
		return nil, err
	}

	// Marshal the inputParams map to JSON
	jsonBytes, err := json.Marshal(inputParams)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal input parameters to JSON: %v", err)
	}

	// Unmarshal the JSON into the dynamic.Message
	if err := reqMsg.UnmarshalJSON(jsonBytes); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON into request message: %v", err)
	}

	fullMethod := fmt.Sprintf("/%s/%s", fullServiceName, verb)

	// Handle client streaming
	if !methodDesc.IsClientStreaming() && methodDesc.IsServerStreaming() {
		streamDesc := &grpc.StreamDesc{
			StreamName:    verb,
			ServerStreams: true,
			ClientStreams: false,
		}

		stream, err := conn.NewStream(ctx, streamDesc, fullMethod)
		if err != nil {
			return nil, fmt.Errorf("failed to create stream: %v", err)
		}

		if err := stream.SendMsg(reqMsg); err != nil {
			return nil, fmt.Errorf("failed to send request message: %v", err)
		}

		if err := stream.CloseSend(); err != nil {
			return nil, fmt.Errorf("failed to close send: %v", err)
		}

		var allResponses []string
		for {
			respMsg := dynamic.NewMessage(methodDesc.GetOutputType())
			err := stream.RecvMsg(respMsg)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to receive response: %v", err)
			}

			jsonBytes, err := respMsg.MarshalJSON()
			if err != nil {
				return nil, fmt.Errorf("failed to marshal response: %v", err)
			}

			allResponses = append(allResponses, string(jsonBytes))
		}

		if len(allResponses) == 1 {
			return []byte(allResponses[0]), nil
		}

		combinedJSON := fmt.Sprintf("{\"results\": [%s]}", strings.Join(allResponses, ","))
		return []byte(combinedJSON), nil
	}

	// Regular unary call
	err = conn.Invoke(ctx, fullMethod, reqMsg, respMsg)
	if err != nil {
		if strings.Contains(err.Error(), "ERROR_AUTHENTICATE_FAILURE") ||
			strings.Contains(err.Error(), "Token is invalid or expired") {

			// Check if current environment is app type
			if strings.HasSuffix(config.Environment, "-app") {
				headerBox := pterm.DefaultBox.WithTitle("App Token Required").
					WithTitleTopCenter().
					WithRightPadding(4).
					WithLeftPadding(4).
					WithBoxStyle(pterm.NewStyle(pterm.FgLightRed))

				appTokenExplain := "Please create a Domain Admin App in SpaceONE Console.\n" +
					"This requires Domain Admin privilege.\n\n" +
					"Or Please create a Workspace App in SpaceONE Console.\n" +
					"This requires Workspace Owner privilege."

				headerBox.Println(appTokenExplain)
				fmt.Println()

				steps := []string{
					"1. Go to SpaceONE Console",
					"2. Navigate to either 'Admin > App Page' or specific 'Workspace > App page'",
					"3. Click 'Create' to create your App",
					"4. Copy the generated App Token",
					fmt.Sprintf("5. Update token in your config file:\n   Path: ~/.cfctl/setting.yaml\n   Environment: %s", config.Environment),
				}

				instructionBox := pterm.DefaultBox.WithTitle("Required Steps").
					WithTitleTopCenter().
					WithRightPadding(4).
					WithLeftPadding(4)

				instructionBox.Println(strings.Join(steps, "\n\n"))

				return nil, fmt.Errorf("app token required")
			} else {
				// Original user authentication error message
				headerBox := pterm.DefaultBox.WithTitle("Authentication Error").
					WithTitleTopCenter().
					WithRightPadding(4).
					WithLeftPadding(4).
					WithBoxStyle(pterm.NewStyle(pterm.FgLightRed))

				errorExplain := "Your authentication token has expired or is invalid.\n" +
					"Please login again to refresh your credentials."

				headerBox.Println(errorExplain)
				fmt.Println()

				steps := []string{
					"1. Run 'cfctl login'",
					"2. Enter your credentials when prompted",
					"3. Try your command again",
				}

				instructionBox := pterm.DefaultBox.WithTitle("Required Steps").
					WithTitleTopCenter().
					WithRightPadding(4).
					WithLeftPadding(4)

				instructionBox.Println(strings.Join(steps, "\n\n"))

				return nil, fmt.Errorf("authentication required")
			}
		}
		return nil, fmt.Errorf("failed to invoke method %s: %v", fullMethod, err)
	}

	return respMsg.MarshalJSON()
}

func parseParameters(options *FetchOptions) (map[string]interface{}, error) {
	parsed := make(map[string]interface{})

	// Load from file parameter if provided
	if options.FileParameter != "" {
		data, err := os.ReadFile(options.FileParameter)
		if err != nil {
			return nil, fmt.Errorf("failed to read file parameter: %v", err)
		}

		var yamlData map[string]interface{}
		if err := yaml.Unmarshal(data, &yamlData); err != nil {
			return nil, fmt.Errorf("failed to unmarshal YAML file: %v", err)
		}

		for key, value := range yamlData {
			switch v := value.(type) {
			case map[string]interface{}:
				// Retain as map instead of converting to Struct
				parsed[key] = v
			case []interface{}:
				// Retain lists as is
				parsed[key] = v
			default:
				parsed[key] = value
			}
		}
	}

	// Load from JSON parameter if provided
	if options.JSONParameter != "" {
		if err := json.Unmarshal([]byte(options.JSONParameter), &parsed); err != nil {
			return nil, fmt.Errorf("failed to unmarshal JSON parameter: %v", err)
		}
	}

	// Parse key=value parameters
	for _, param := range options.Parameters {
		parts := strings.SplitN(param, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid parameter format. Use key=value")
		}
		key := parts[0]
		value := parts[1]

		// Attempt to parse value as JSON
		var jsonValue interface{}
		if err := json.Unmarshal([]byte(value), &jsonValue); err == nil {
			parsed[key] = jsonValue
		} else {
			parsed[key] = value
		}
	}

	return parsed, nil
}

func discoverService(refClient *grpcreflect.Client, serviceName string, resourceName string) (string, error) {
	services, err := refClient.ListServices()
	if err != nil {
		return "", fmt.Errorf("failed to list services: %v", err)
	}

	for _, service := range services {
		if strings.Contains(service, ".plugin.") && strings.HasSuffix(service, resourceName) {
			return service, nil
		}
	}

	for _, service := range services {
		if strings.Contains(service, fmt.Sprintf("spaceone.api.%s", serviceName)) &&
			strings.HasSuffix(service, resourceName) {
			return service, nil
		}
	}

	return "", fmt.Errorf("service not found for %s.%s", serviceName, resourceName)
}

// WatchResource monitors a resource for changes and prints updates
func WatchResource(serviceName, verb, resource string, options *FetchOptions) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)

	seenItems := make(map[string]bool)

	initialData, err := FetchService(serviceName, verb, resource, &FetchOptions{
		Parameters:      options.Parameters,
		JSONParameter:   options.JSONParameter,
		FileParameter:   options.FileParameter,
		APIVersion:      options.APIVersion,
		OutputFormat:    "",
		CopyToClipboard: false,
	})
	if err != nil {
		return err
	}

	if results, ok := initialData["results"].([]interface{}); ok {
		var recentItems []map[string]interface{}

		for _, item := range results {
			if m, ok := item.(map[string]interface{}); ok {
				identifier := format.GenerateIdentifier(m)
				seenItems[identifier] = true

				recentItems = append(recentItems, m)
				if len(recentItems) > 20 {
					recentItems = recentItems[1:]
				}
			}
		}

		if len(recentItems) > 0 {
			fmt.Printf("Recent items:\n")
			format.PrintNewItems(recentItems)
		}
	}

	fmt.Printf("\nWatching for changes... (Ctrl+C to quit)\n\n")

	for {
		select {
		case <-ticker.C:
			newData, err := FetchService(serviceName, verb, resource, &FetchOptions{
				Parameters:      options.Parameters,
				JSONParameter:   options.JSONParameter,
				FileParameter:   options.FileParameter,
				APIVersion:      options.APIVersion,
				OutputFormat:    "",
				CopyToClipboard: false,
			})
			if err != nil {
				continue
			}

			var newItems []map[string]interface{}
			if results, ok := newData["results"].([]interface{}); ok {
				for _, item := range results {
					if m, ok := item.(map[string]interface{}); ok {
						identifier := format.GenerateIdentifier(m)
						if !seenItems[identifier] {
							newItems = append(newItems, m)
							seenItems[identifier] = true
						}
					}
				}
			}

			if len(newItems) > 0 {
				fmt.Printf("Found %d new items at %s:\n",
					len(newItems),
					time.Now().Format("2006-01-02 15:04:05"))

				format.PrintNewItems(newItems)
				fmt.Println()
			}

		case <-sigChan:
			fmt.Println("\nStopping watch...")
			return nil
		}
	}
}

func printData(data map[string]interface{}, options *FetchOptions, serviceName, verbName, resourceName string, refClient *grpcreflect.Client) {
	var output string

	switch options.OutputFormat {
	case "json":
		dataBytes, err := json.MarshalIndent(data, "", "  ")
		if err != nil {
			log.Fatalf("Failed to marshal response to JSON: %v", err)
		}
		output = string(dataBytes)
		fmt.Println(output)

	case "yaml":
		if results, ok := data["results"].([]interface{}); ok && len(results) > 0 {
			var sb strings.Builder

			for i, item := range results {
				if i > 0 {
					sb.WriteString("---\n")
				}
				sb.WriteString(printYAMLDoc(item))
			}
			output = sb.String()
			fmt.Print(output)
		} else {
			output = printYAMLDoc(data)
			fmt.Print(output)
		}

	case "table":
		output = printTable(data, options, serviceName, verbName, resourceName, refClient)

	case "csv":
		output = printCSV(data)

	default:
		output = printYAMLDoc(data)
		fmt.Print(output)
	}

	// Copy to clipboard if requested
	if options.CopyToClipboard && output != "" {
		if err := clipboard.WriteAll(output); err != nil {
			log.Fatalf("Failed to copy to clipboard: %v", err)
		}
		pterm.Success.Println("The output has been copied to your clipboard.")
	}
}

func printYAMLDoc(v interface{}) string {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(v); err != nil {
		log.Fatalf("Failed to marshal response to YAML: %v", err)
	}
	return buf.String()
}

func getMinimalFields(serviceName, resourceName string, refClient *grpcreflect.Client) []string {
	// Default minimal fields that should always be included if they exist
	defaultFields := []string{"name", "created_at"}

	// Try to get message descriptor for the resource
	fullServiceName := fmt.Sprintf("spaceone.api.%s.v1.%s", serviceName, resourceName)
	serviceDesc, err := refClient.ResolveService(fullServiceName)
	if err != nil {
		// Try v2 if v1 fails
		fullServiceName = fmt.Sprintf("spaceone.api.%s.v2.%s", serviceName, resourceName)
		serviceDesc, err = refClient.ResolveService(fullServiceName)
		if err != nil {
			return defaultFields
		}
	}

	// Get list method descriptor
	listMethod := serviceDesc.FindMethodByName("list")
	if listMethod == nil {
		return defaultFields
	}

	// Get response message descriptor
	respDesc := listMethod.GetOutputType()
	if respDesc == nil {
		return defaultFields
	}

	// Find the 'results' field which should be repeated message type
	resultsField := respDesc.FindFieldByName("results")
	if resultsField == nil {
		return defaultFields
	}

	// Get the message type of items in the results
	itemMsgDesc := resultsField.GetMessageType()
	if itemMsgDesc == nil {
		return defaultFields
	}

	// Collect required fields and important fields
	minimalFields := make([]string, 0)
	fields := itemMsgDesc.GetFields()
	for _, field := range fields {
		// Add ID fields
		if strings.HasSuffix(field.GetName(), "_id") {
			minimalFields = append(minimalFields, field.GetName())
			continue
		}

		// Add status/state fields
		if field.GetName() == "status" || field.GetName() == "state" {
			minimalFields = append(minimalFields, field.GetName())
			continue
		}

		// Add timestamp fields
		if field.GetName() == "created_at" || field.GetName() == "finished_at" {
			minimalFields = append(minimalFields, field.GetName())
			continue
		}

		// Add name field
		if field.GetName() == "name" {
			minimalFields = append(minimalFields, field.GetName())
			continue
		}
	}

	if len(minimalFields) == 0 {
		return defaultFields
	}

	return minimalFields
}

func printTable(data map[string]interface{}, options *FetchOptions, serviceName, verbName, resourceName string, refClient *grpcreflect.Client) string {
	if results, ok := data["results"].([]interface{}); ok {
		// Set default page size if not specified and paging is enabled
		if !options.NoPaging {
			if options.PageSize == 0 {
				options.PageSize = 15
			}
		} else {
			// Show all results when no-paging is true
			options.PageSize = len(results)
		}

		// Initialize keyboard
		if err := keyboard.Open(); err != nil {
			fmt.Println("Failed to initialize keyboard:", err)
			return ""
		}
		defer keyboard.Close()

		currentPage := 0
		searchTerm := ""
		filteredResults := results

		// Extract headers
		headers := make(map[string]bool)
		for _, result := range results[:min(1000, len(results))] {
			if row, ok := result.(map[string]interface{}); ok {
				for key := range row {
					headers[key] = true
				}
			}
		}

		// Convert headers to sorted slice
		headerSlice := make([]string, 0, len(headers))
		for key := range headers {
			headerSlice = append(headerSlice, key)
		}
		sort.Strings(headerSlice)

		// Handle minimal columns
		if options.MinimalColumns {
			minimalFields := getMinimalFields(serviceName, resourceName, refClient)
			var minimalHeaderSlice []string
			for _, field := range minimalFields {
				if headers[field] {
					minimalHeaderSlice = append(minimalHeaderSlice, field)
				}
			}
			if len(minimalHeaderSlice) > 0 {
				headerSlice = minimalHeaderSlice
			}
		}

		for {
			if searchTerm != "" {
				filteredResults = filterResults(results, searchTerm)
			} else {
				filteredResults = results
			}

			totalItems := len(filteredResults)
			totalPages := (totalItems + options.PageSize - 1) / options.PageSize

			tableData := pterm.TableData{headerSlice}

			// Calculate page items
			startIdx := currentPage * options.PageSize
			endIdx := startIdx + options.PageSize
			if endIdx > totalItems {
				endIdx = totalItems
			}

			// Clear screen
			fmt.Print("\033[H\033[2J")

			if searchTerm != "" {
				fmt.Printf("Search: %s (Found: %d items)\n", searchTerm, totalItems)
			}

			// Add rows for current page
			pageResults := filteredResults[startIdx:endIdx]
			for _, result := range pageResults {
				if row, ok := result.(map[string]interface{}); ok {
					rowData := make([]string, len(headerSlice))
					for i, key := range headerSlice {
						rowData[i] = FormatTableValue(row[key])
					}
					tableData = append(tableData, rowData)
				}
			}

			// Print table
			pterm.DefaultTable.WithHasHeader().WithData(tableData).Render()

			fmt.Printf("\nPage %d of %d (Total items: %d)\n", currentPage+1, totalPages, totalItems)
			fmt.Println("Navigation: [h]previous page, [l]next page, [/]search, [c]lear search, [q]uit")

			// Handle keyboard input
			char, _, err := keyboard.GetKey()
			if err != nil {
				fmt.Println("Error reading keyboard input:", err)
				return ""
			}

			switch char {
			case 'l', 'L':
				currentPage = (currentPage + 1) % totalPages
			case 'h', 'H':
				currentPage = (currentPage - 1 + totalPages) % totalPages
			case 'q', 'Q':
				return ""
			case 'c', 'C':
				searchTerm = ""
				currentPage = 0
			case '/':
				fmt.Print("\nEnter search term: ")
				keyboard.Close()
				var input string
				fmt.Scanln(&input)
				searchTerm = input
				currentPage = 0
				keyboard.Open()
			}
		}
	}

	// Handle non-list results
	headers := make([]string, 0)
	for key := range data {
		headers = append(headers, key)
	}
	sort.Strings(headers)

	tableData := pterm.TableData{
		{"Field", "Value"},
	}

	for _, header := range headers {
		value := FormatTableValue(data[header])
		tableData = append(tableData, []string{header, value})
	}

	pterm.DefaultTable.WithHasHeader().WithData(tableData).Render()
	return ""
}

func filterResults(results []interface{}, searchTerm string) []interface{} {
	var filtered []interface{}
	searchTerm = strings.ToLower(searchTerm)

	for _, result := range results {
		if row, ok := result.(map[string]interface{}); ok {
			for _, value := range row {
				strValue := strings.ToLower(fmt.Sprintf("%v", value))
				if strings.Contains(strValue, searchTerm) {
					filtered = append(filtered, result)
					break
				}
			}
		}
	}
	return filtered
}

func FormatTableValue(val interface{}) string {
	switch v := val.(type) {
	case nil:
		return ""
	case string:
		// Add colors for status values
		switch strings.ToUpper(v) {
		case "SUCCESS":
			return pterm.FgGreen.Sprint(v)
		case "FAILURE":
			return pterm.FgRed.Sprint(v)
		case "PENDING":
			return pterm.FgYellow.Sprint(v)
		case "RUNNING":
			return pterm.FgBlue.Sprint(v)
		default:
			return v
		}
	case float64, float32, int, int32, int64, uint, uint32, uint64:
		return fmt.Sprintf("%v", v)
	case bool:
		return fmt.Sprintf("%v", v)
	case map[string]interface{}, []interface{}:
		jsonBytes, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(jsonBytes)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func printCSV(data map[string]interface{}) string {
	writer := csv.NewWriter(os.Stdout)
	defer writer.Flush()

	if results, ok := data["results"].([]interface{}); ok {
		if len(results) == 0 {
			return ""
		}

		headers := make([]string, 0)
		if firstRow, ok := results[0].(map[string]interface{}); ok {
			for key := range firstRow {
				headers = append(headers, key)
			}
			sort.Strings(headers)
			writer.Write(headers)
		}

		for _, result := range results {
			if row, ok := result.(map[string]interface{}); ok {
				rowData := make([]string, len(headers))
				for i, header := range headers {
					rowData[i] = FormatTableValue(row[header])
				}
				writer.Write(rowData)
			}
		}
	} else {
		headers := []string{"Field", "Value"}
		writer.Write(headers)

		fields := make([]string, 0)
		for field := range data {
			fields = append(fields, field)
		}
		sort.Strings(fields)

		for _, field := range fields {
			row := []string{field, FormatTableValue(data[field])}
			writer.Write(row)
		}
	}

	return ""
}

func formatCSVValue(val interface{}) string {
	switch v := val.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64, float32, int, int32, int64, uint, uint32, uint64:
		return fmt.Sprintf("%v", v)
	case bool:
		return fmt.Sprintf("%v", v)
	case map[string]interface{}, []interface{}:
		jsonBytes, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(jsonBytes)
	default:
		return fmt.Sprintf("%v", v)
	}
}
