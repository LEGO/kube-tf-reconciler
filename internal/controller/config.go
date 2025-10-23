package controller

type Config struct {
	Port                 string
	ProbeAddr            string
	LogLevel             string
	Namespace            string
	NamespaceLabel       string
	LeaderElectionID     string
	EnableLeaderElection bool
	WorkspacePath        string
}

func DefaultConfig() Config {
	return Config{
		Port:                 ":8080",
		ProbeAddr:            ":8081",
		LeaderElectionID:     "69943c0d.krec-operator.lego.com",
		Namespace:            "krec",
		EnableLeaderElection: false,
		WorkspacePath:        "./.testdata",
	}
}
