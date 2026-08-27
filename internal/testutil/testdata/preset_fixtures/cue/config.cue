package fixture

import (
	"strings"
	"list"
)

#Service: {
	name:     string
	port:     int & >0 & <65536
	replicas: int | *1
}

#Cluster: {
	region:   string
	services: [...#Service]
}

let domain = "mache.example.com"

production: #Cluster & {
	region: "us-east-1"
	services: [
		{name: "api", port: 8080, replicas: 3},
		{name: "worker", port: 9090},
	]
}

staging: #Cluster & {
	region: "us-west-2"
	services: production.services
}

allNames: [for s in production.services {strings.ToUpper(s.name)}]
allPorts: list.SortStrings([for s in production.services {"\(s.port)"}])

apex:   domain
canary: "canary.\(domain)"
