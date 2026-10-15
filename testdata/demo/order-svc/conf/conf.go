package conf

// Names this service registers under and calls.
const (
	ServiceName     = "order"
	UserServiceName = "user"
)

// Config is loaded from a YAML file at startup.
type Config struct {
	PayService string
}

// Get returns the loaded configuration.
func Get() *Config { return &Config{PayService: "pay"} }
