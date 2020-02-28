package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"
)

var (
	stAPI       models.API
	gostServer  http.Server
	mqttClient  models.MQTTClient
	file        *os.File
	logger      *log.Logger
	mainLogger  *log.Entry
	conf        configuration.Config
	cfgFlag     = flag.String("config", "config.yaml", "path of the config file")
	installFlag = flag.String("install", "", "path to the database creation file")
)

func initialize() {
	flag.Parse()
	cfg := *cfgFlag
	var err error
	conf, err = configuration.GetConfig(cfg)
	if err != nil {
		log.Fatal("config read error: ", err)
		return
	}

	configuration.SetEnvironmentVariables(&conf)
	logger, err := gostLog.InitializeLogger(file, conf.Logger.FileName, &log.TextFormatter{FullTimestamp: true}, conf.Logger.Verbose)
	if err != nil {
		log.Println("Error initializing logger, defaulting to stdout. Error: " + err.Error())
	}

	// Setting default fields for main logger
	mainLogger = logger.WithFields(log.Fields{"package": "main"})
}

func main() {
	initialize()
	stop := make(chan os.Signal, 2)
	signal.Notify(stop, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		<-stop
		// mainLogger.Info("GOST stopped gracefully")
		cleanup()
		os.Exit(1)
	}()

	mainLogger.Info("Starting GOST")

	database := postgis.NewDatabase(
		conf.Database.Host,
		conf.Database.Port,
		conf.Database.User,
		conf.Database.Password,
		conf.Database.Database,
		conf.Database.Schema,
		conf.Database.SSL,
		conf.Database.MaxIdleConns,
		conf.Database.MaxOpenConns,
		conf.Server.MaxEntityResponse)
	go database.Start()

	// if install is supplied create database and close, if not start server
	sqlFile := *installFlag
	if len(sqlFile) != 0 {
		createDatabase(database, sqlFile)
	} else {
		mqttClient = mqtt.CreateMQTTClient(conf.MQTT)
		stAPI = api.NewAPI(database, conf, mqttClient)

		if conf.MQTT.Enabled {
			mqttClient.Start(&stAPI)
		}

		createAndStartServer(&stAPI)
	}
}

func createDatabase(db models.Database, sqlFile string) {
	mainLogger.Info("CREATING DATABASE")

	err := db.CreateSchema(sqlFile)
	if err != nil {
		mainLogger.Fatal(err)
	}

	mainLogger.Info("Database created successfully, you can start your server now")
}

// createAndStartServer creates the GOST HTTPServer and starts it
func createAndStartServer(api *models.API) {
	a := *api
	a.Start()

	config := a.GetConfig()
	gostServer = http.CreateServer(
		config.Server.Host,
		config.Server.Port,
		api,
		config.Server.HTTPS,
		config.Server.HTTPSCert,
		config.Server.HTTPSKey)
	gostServer.Start()
}

func cleanup() {
	if gostServer != nil {
		gostServer.Stop()
	}

	gostLog.CleanUp()
}
