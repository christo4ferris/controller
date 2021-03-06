// Copyright 2016 IBM Corporation
//
//   Licensed under the Apache License, Version 2.0 (the "License");
//   you may not use this file except in compliance with the License.
//   You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
//   Unless required by applicable law or agreed to in writing, software
//   distributed under the License is distributed on an "AS IS" BASIS,
//   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//   See the License for the specific language governing permissions and
//   limitations under the License.

package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/Sirupsen/logrus"
	"github.com/amalgam8/controller/api"
	"github.com/amalgam8/controller/auth"
	"github.com/amalgam8/controller/config"
	"github.com/amalgam8/controller/database"
	"github.com/amalgam8/controller/manager"
	"github.com/amalgam8/controller/metrics"
	"github.com/amalgam8/controller/middleware"
	"github.com/ant0ine/go-json-rest/rest"
	"github.com/codegangsta/cli"
)

func main() {
	app := cli.NewApp()

	app.Name = "controller"
	app.Usage = "Amalgam8 Controller"
	app.Version = "0.2.0"
	app.Flags = config.Flags
	app.Action = controllerCommand

	err := app.Run(os.Args)
	if err != nil {
		logrus.WithError(err).Error("Command setup failed")
	}
}

func controllerCommand(context *cli.Context) {
	conf := config.New(context)
	if err := controllerMain(*conf); err != nil {
		logrus.WithError(err).Error("Controller setup failed")
		logrus.Warn("Simple error server running...")
		select {}
	}
}

func controllerMain(conf config.Config) error {
	var err error

	logrus.ErrorKey = "error"
	logrus.Info(conf.LogLevel)
	logrus.SetLevel(conf.LogLevel)

	setupHandler := middleware.NewSetupHandler()

	var validationErr error
	if validationErr = conf.Validate(); validationErr != nil {
		logrus.WithError(validationErr).Error("Validation of config failed")
		setupHandler.SetError(validationErr)
	}

	go func() {
		if err := http.ListenAndServe(fmt.Sprintf(":%v", conf.APIPort), setupHandler); err != nil {
			logrus.WithError(err).Error("Server init failed")
		}
	}()

	if validationErr != nil {
		setupHandler.SetError(validationErr)
		return validationErr
	}

	reporter := metrics.NewReporter()

	var tenantDB database.Tenant
	if conf.Database.Type == "memory" {

		db := database.NewMemoryCloudantDB()
		tenantDB = database.NewTenant(db)
	} else if conf.Database.Type == "cloudant" {
		logrus.Warn("Cloudant currently not supported, using in memory storage")
		db := database.NewMemoryCloudantDB()
		tenantDB = database.NewTenant(db)
	} else {
		err = errors.New("unsupported database type")
		setupHandler.SetError(err)
		return err
	}

	r := manager.NewManager(manager.Config{
		Database: tenantDB,
	})

	tenantAPI := api.NewTenant(api.TenantConfig{
		Reporter: reporter,
		Manager:  r,
	})
	healthAPI := api.NewHealth(reporter)

	var authenticator auth.Authenticator
	if len(conf.AuthModes) > 0 {
		auths := make([]auth.Authenticator, len(conf.AuthModes))
		for i, mode := range conf.AuthModes {
			switch mode {
			case "trusted":
				auths[i] = auth.NewTrustedAuthenticator()
			case "jwt":
				jwtAuth, err := auth.NewJWTAuthenticator([]byte(conf.JWTSecret))
				if err != nil {
					return fmt.Errorf("Failed to create the authentication module: %s", err)
				}
				auths[i] = jwtAuth
			default:
				return fmt.Errorf("Failed to create the authentication module: unrecognized authentication mode '%s'", err)
			}
		}
		authenticator, err = auth.NewChainAuthenticator(auths)
		if err != nil {
			return err
		}
	} else {
		authenticator = auth.DefaultAuthenticator()
	}

	a := rest.NewApi()
	a.Use(
		&rest.TimerMiddleware{},
		&rest.RecorderMiddleware{},
		&rest.RecoverMiddleware{
			EnableResponseStackTrace: false,
		},
		&rest.ContentTypeCheckerMiddleware{},
		&middleware.RequestIDMiddleware{},
		&middleware.LoggingMiddleware{},
		middleware.NewRequireHTTPS(middleware.CheckRequest{
			IsSecure: middleware.IsUsingSecureConnection,
			Disabled: !conf.RequireHTTPS,
		}),
	)

	authMw := &middleware.AuthMiddleware{Authenticator: authenticator}

	routes := tenantAPI.Routes(authMw)
	routes = append(routes, healthAPI.Routes()...)

	router, err := rest.MakeRouter(
		routes...,
	)
	if err != nil {
		setupHandler.SetError(err)
		return err
	}
	a.SetApp(router)

	setupHandler.SetHandler(a.MakeHandler())

	// Server is already started
	logrus.WithFields(logrus.Fields{
		"port": conf.APIPort,
	}).Info("Server started")

	select {}
}
