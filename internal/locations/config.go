package locations

import (
	"fmt"
	"os"
	"time"
)

// Config is the location service's settings, from environment variables.
type Config struct {
	// HTTPAddr serves the WebSocket door and the stats and health pages. UDPAddr
	// is the UDP door.
	HTTPAddr string
	UDPAddr  string
	// RedisURL is where the live map is.
	RedisURL string
	// ShiftKeySecret checks the signature on each ping. It's the same secret the
	// API makes shift keys with.
	ShiftKeySecret  string
	ShutdownTimeout time.Duration
}

// LoadConfig reads and checks the settings.
func LoadConfig() (Config, error) {
	cfg := Config{
		HTTPAddr:        getenv("HTTP_ADDR", ":8095"),
		UDPAddr:         getenv("UDP_ADDR", ":8095"),
		RedisURL:        os.Getenv("REDIS_URL"),
		ShiftKeySecret:  os.Getenv("SHIFT_KEY_SECRET"),
		ShutdownTimeout: 10 * time.Second,
	}
	if cfg.RedisURL == "" {
		return Config{}, fmt.Errorf("REDIS_URL is required: the live map lives there")
	}
	if len(cfg.ShiftKeySecret) < 32 {
		return Config{}, fmt.Errorf("SHIFT_KEY_SECRET is required and must be at least 32 characters: the same one the API has")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
