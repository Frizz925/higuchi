package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/frizz925/higuchi/internal/auth"
	"github.com/frizz925/higuchi/internal/config"
	"github.com/frizz925/higuchi/internal/crypto/hasher"
	"github.com/frizz925/higuchi/internal/dispatcher"
	"github.com/frizz925/higuchi/internal/filter"
	"github.com/frizz925/higuchi/internal/pool"
	"github.com/frizz925/higuchi/internal/server"
	"github.com/frizz925/higuchi/internal/worker"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

const unixAddressPrefix = "unix:"

var unixAddressPrefixLength = len(unixAddressPrefix)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the web proxy server",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runServe()
	},
}

func runServe() error {
	cfg, err := config.ReadConfig()
	if err != nil {
		return fmt.Errorf("error while reading config: %v", err)
	}
	logger := cfg.Logger.Create()
	defer logger.Sync()

	pepper, err := cfg.Filters.Auth.Pepper()
	if err != nil {
		return fmt.Errorf("error while decoding pepper: %v", err)
	}

	authCompare := func(password string, i interface{}) bool {
		switch v := i.(type) {
		case string:
			return password == v
		case hasher.Argon2Digest:
			return v.Compare(password) == 0
		}
		return false
	}
	users := make(map[string]interface{})

	if cfg.Filters.Auth.Enabled {
		h := hasher.NewArgon2Hasher(pepper)
		aa := auth.NewArgon2Auth(h)
		au, err := aa.ReadPasswordsFile(cfg.Filters.Auth.PasswordsFile)
		if err != nil {
			return fmt.Errorf("error while reading passwords file: %v", err)
		}
		for user, ad := range au {
			users[user] = ad
		}
	}

	var certbotConfig filter.CertbotConfig
	if cfg.Filters.Certbot.Enabled {
		certbotConfig.Hostname = cfg.Filters.Certbot.Hostname
		certbotConfig.Webroot = cfg.Filters.Certbot.Webroot
		certbotConfig.ChallengePath = cfg.Filters.Certbot.ChallengePath
	}

	s := server.New(server.Config{
		Logger: logger,
		Pool: pool.NewPreallocatedPool(func(num int) *worker.Worker {
			hfs := []filter.HTTPFilter{}
			if cfg.Filters.Forwarded.Enabled {
				hfs = append(hfs, filter.DefaultForwardedFilter)
			}
			if cfg.Filters.Certbot.Enabled {
				hfs = append(hfs, filter.NewCertbotFilter(certbotConfig))
			}
			if cfg.Filters.Auth.Enabled {
				hfs = append(hfs, filter.NewAuthFilter(users, authCompare))
			}
			df := filter.NewDispatchFilter(dispatcher.NewTCPDispatcher(cfg.Worker.BufferSize))
			hfs = append(
				hfs,
				filter.NewTunnelFilter(cfg.Worker.BufferSize),
				filter.NewForwardFilter(df),
			)

			filters := []filter.Filter{}
			if cfg.Filters.Healthcheck.Enabled {
				filters = append(filters, filter.NewHealthCheckFilter(
					cfg.Filters.Healthcheck.Method,
					cfg.Filters.Healthcheck.Path,
				))
			}
			filters = append(filters, filter.NewParseFilter(hfs...))
			return worker.New(num, filters...)
		}, 1024),
	})

	for _, addr := range cfg.Server.Listeners {
		network := "tcp"
		if strings.HasPrefix(addr, unixAddressPrefix) {
			network = "unix"
			addr = addr[unixAddressPrefixLength:]
		}
		if _, err := s.Listen(network, addr); err != nil {
			return err
		}
		logger.Info(fmt.Sprintf("Higuchi listening at %s", addr))
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("Received stop signal", zap.String("signal", sig.String()))

	return s.Close()
}
