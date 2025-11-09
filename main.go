package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"

	"github.com/perimeter-81/proxmox-api-go/cli"
	_ "github.com/perimeter-81/proxmox-api-go/cli/command/commands"
	"github.com/perimeter-81/proxmox-api-go/proxmox"
)

func main() {
	if os.Getenv("NEW_CLI") == "true" {
		err := cli.Execute()
		if err != nil {
			failError(err)
		}
		os.Exit(0)
	}
	insecure := flag.Bool("insecure", false, "TLS insecure mode")
	proxmox.Debug = flag.Bool("debug", false, "debug mode")
	fConfigFile := flag.String("file", "", "file to get the config from")
	taskTimeout := flag.Int("timeout", 300, "api task timeout in seconds")
	proxyURL := flag.String("proxy", "", "proxy url to connect to")
	fvmid := flag.Int("vmid", -1, "custom vmid (instead of auto)")
	flag.Parse()
	tlsconf := &tls.Config{InsecureSkipVerify: true}
	if !*insecure {
		tlsconf = nil
	}
	c, err := proxmox.NewClient(os.Getenv("PM_API_URL"), nil, os.Getenv("PM_HTTP_HEADERS"), tlsconf, *proxyURL, *taskTimeout)
	failError(err)
	if userRequiresAPIToken(os.Getenv("PM_USER")) {
		c.SetAPIToken(os.Getenv("PM_USER"), os.Getenv("PM_PASS"))
		// As test, get the version of the server
		_, err := c.GetVersion(context.Background())
		if err != nil {
			log.Fatalf("login error: %s", err)
		}
	} else {
		err = c.Login(os.Getenv("PM_USER"), os.Getenv("PM_PASS"), os.Getenv("PM_OTP"))
		failError(err)
	}

	vmid := *fvmid
	if vmid < 0 {
		if len(flag.Args()) > 1 {
			vmid, err = strconv.Atoi(flag.Args()[1])
			if err != nil {
				vmid = 0
			}
		} else if len(flag.Args()) == 0 || (flag.Args()[0] == "idstatus") {
			vmid = 0
		}
	}

	var jbody interface{}
	var vmr *proxmox.VmRef

	if len(flag.Args()) == 0 {
		fmt.Printf("Missing action, try start|stop vmid\n")
		os.Exit(0)
	}

	switch flag.Args()[0] {

	// TODO make testUserPermissions in new cli
	case "testUserPermissions":
		// testuserpermission [user] [path]
		// ex: testuserpermission root@pam(default) /(default)
		var testpath string
		var testUser proxmox.UserID
		if len(flag.Args()) < 2 {
			testUser, err = proxmox.NewUserID(os.Getenv("PM_USER"))
		} else {
			testUser, err = proxmox.NewUserID(flag.Args()[1])
		}
		failError(err)
		if len(flag.Args()) < 3 {
			testpath = ""
		} else {
			testpath = flag.Args()[2]
		}
		permissions, err := c.GetUserPermissions(context.Background(), testUser, testpath)
		failError(err)
		sort.Strings(permissions)
		log.Println(permissions)

	case "start":
		vmr = proxmox.NewVmRef(vmid)
		jbody, err = c.StartVm(context.Background(), vmr)
		failError(err)

	case "stop":

		vmr = proxmox.NewVmRef(vmid)
		jbody, err = c.StopVm(context.Background(), vmr)
		failError(err)

	case "destroy":
		vmr = proxmox.NewVmRef(vmid)
		jbody, err = c.StopVm(context.Background(), vmr)
		failError(err)
		jbody, err = c.DeleteVm(context.Background(), vmr)
		failError(err)

	case "getConfig":
		vmr = proxmox.NewVmRef(vmid)
		err := c.CheckVmRef(context.Background(), vmr)
		failError(err)
		vmType := vmr.GetVmType()
		var config interface{}
		switch vmType {
		case "qemu":
			config, err = proxmox.NewConfigQemuFromApi(context.Background(), vmr, c)
		case "lxc":
			config, err = proxmox.NewConfigLxcFromApi(vmr, c)
		}
		failError(err)
		cj, err := json.MarshalIndent(config, "", "  ")
		failError(err)
		log.Println(string(cj))
		// TODO make getNetworkInterfaces in new cli
	case "getNetworkInterfaces":
		vmr = proxmox.NewVmRef(vmid)
		err := c.CheckVmRef(context.Background(), vmr)
		failError(err)
		networkInterfaces, err := c.GetVmAgentNetworkInterfaces(context.Background(), vmr)
		failError(err)

		networkInterfaceJSON, err := json.Marshal(networkInterfaces)
		failError(err)
		fmt.Println(string(networkInterfaceJSON))

	case "createQemu":
		config, err := proxmox.NewConfigQemuFromJson(GetConfig(*fConfigFile))
		failError(err)
		vmr = proxmox.NewVmRef(vmid)
		vmr.SetNode(flag.Args()[2])
		failError(config.CreateVm(context.Background(), vmr, c))
		log.Println("Complete")

	case "createLxc":
		config, err := proxmox.NewConfigLxcFromJson(GetConfig(*fConfigFile))
		failError(err)
		vmr = proxmox.NewVmRef(vmid)
		vmr.SetNode(flag.Args()[2])
		failError(config.CreateLxc(vmr, c))
		log.Println("Complete")
		// TODO make installQemu in new cli
	case "installQemu":
		config, err := proxmox.NewConfigQemuFromJson(GetConfig(*fConfigFile))
		var mode string
		if config.QemuIso != "" {
			mode = "(ISO boot mode)"
		} else if config.QemuPxe {
			mode = "(PXE boot mode)"
		}
		failError(err)
		if vmid > 0 {
			vmr = proxmox.NewVmRef(vmid)
		} else {
			nextid, err := c.GetNextID(context.Background(), 0)
			failError(err)
			vmr = proxmox.NewVmRef(nextid)
		}
		vmr.SetNode(flag.Args()[1])
		log.Printf("Creating node %s: \n", mode)
		log.Println(vmr)
		failError(config.CreateVm(context.Background(), vmr, c))
		_, err = c.StartVm(context.Background(), vmr)
		failError(err)

		// ISO mode waits for the VM to reboot to exit
		// while PXE mode just launches the VM and is done
		if config.QemuIso != "" {
			_, err := proxmox.SshForwardUsernet(context.Background(), vmr, c)
			failError(err)
			log.Println("Waiting for CDRom install shutdown (at least 5 minutes)")
			failError(proxmox.WaitForShutdown(context.Background(), vmr, c))
			log.Println("Restarting")
			_, err = c.StartVm(context.Background(), vmr)
			failError(err)
			_, err = proxmox.SshForwardUsernet(context.Background(), vmr, c)
			failError(err)
			//log.Println("SSH Portforward on:" + sshPort)
		}

		log.Println("Complete")

	case "idstatus":
		maxid, err := proxmox.MaxVmId(context.Background(), c)
		failError(err)
		nextid, err := c.GetNextID(context.Background(), vmid)
		failError(err)
		log.Println("---")
		log.Printf("MaxID: %d\n", maxid)
		log.Printf("NextID: %d\n", nextid)
		log.Println("---")
		// TODO make cloneQemu in new cli
	case "cloneQemu":
		config, err := proxmox.NewConfigQemuFromJson(GetConfig(*fConfigFile))
		failError(err)
		fmt.Println("Parsed conf: ", config)
		log.Println("Looking for template: " + flag.Args()[1])
		sourceVmrs, err := c.GetVmRefsByName(context.Background(), flag.Args()[1])
		failError(err)
		if sourceVmrs == nil {
			log.Fatal("Can't find template")
			return
		}
		if vmid == 0 {
			vmid, err = c.GetNextID(context.Background(), 0)
			failError(err)
		}
		vmr = proxmox.NewVmRef(vmid)
		vmr.SetNode(flag.Args()[2])
		log.Print("Creating node: ")
		log.Println(vmr)
		// prefer source Vm located on same node
		sourceVmr := sourceVmrs[0]
		for _, candVmr := range sourceVmrs {
			if candVmr.Node() == vmr.Node() {
				sourceVmr = candVmr
			}
		}

		failError(config.CloneVm(context.Background(), sourceVmr, vmr, c))
		failError(config.UpdateConfig(context.Background(), vmr, c))
		log.Println("Complete")

	case "createQemuSnapshot":
		sourceVmr, err := c.GetVmRefByName(context.Background(), flag.Args()[1])
		failError(err)
		jbody, err = c.CreateQemuSnapshot(context.Background(), sourceVmr, flag.Args()[2])
		failError(err)

	case "deleteQemuSnapshot":
		sourceVmr, err := c.GetVmRefByName(context.Background(), flag.Args()[1])
		failError(err)
		jbody, err = c.DeleteQemuSnapshot(context.Background(), sourceVmr, flag.Args()[2])
		failError(err)

	case "listQemuSnapshot":
		sourceVmr, err := c.GetVmRefByName(context.Background(), flag.Args()[1])
		if err == nil {
			jbody, _, err = c.ListQemuSnapshot(context.Background(), sourceVmr)
			if rec, ok := jbody.(map[string]interface{}); ok {
				temp := rec["data"].([]interface{})
				for _, val := range temp {
					snapshotName := val.(map[string]interface{})
					if snapshotName["name"] != "current" {
						fmt.Println(snapshotName["name"])
					}
				}
			} else {
				fmt.Printf("record not a map[string]interface{}: %v\n", jbody)
			}
		}
		failError(err)

	case "listQemuSnapshot2":
		sourceVmrs, err := c.GetVmRefsByName(context.Background(), flag.Args()[1])
		if err == nil {
			for _, sourceVmr := range sourceVmrs {
				jbody, _, err = c.ListQemuSnapshot(context.Background(), sourceVmr)
				if rec, ok := jbody.(map[string]interface{}); ok {
					temp := rec["data"].([]interface{})
					for _, val := range temp {
						snapshotName := val.(map[string]interface{})
						if snapshotName["name"] != "current" {
							fmt.Printf("%d@%s:%s\n", sourceVmr.VmId(), sourceVmr.Node(), snapshotName["name"])
						}
					}
				} else {
					fmt.Printf("record not a map[string]interface{}: %v\n", jbody)
				}
			}
		}
		failError(err)

	case "rollbackQemu":
		sourceVmr, err := c.GetVmRefByName(context.Background(), flag.Args()[1])
		failError(err)
		jbody, err = c.RollbackQemuVm(context.Background(), sourceVmr, flag.Args()[2])
		failError(err)
		// TODO make sshforward in new cli
	case "sshforward":
		vmr = proxmox.NewVmRef(vmid)
		sshPort, err := proxmox.SshForwardUsernet(context.Background(), vmr, c)
		failError(err)
		log.Println("SSH Portforward on:" + sshPort)
		// TODO make sshbackward in new cli
	case "sshbackward":
		vmr = proxmox.NewVmRef(vmid)
		err = proxmox.RemoveSshForwardUsernet(context.Background(), vmr, c)
		failError(err)
		log.Println("SSH Portforward off")
		// TODO make sendstring in new cli
	case "sendstring":
		vmr = proxmox.NewVmRef(vmid)
		err = proxmox.SendKeysString(context.Background(), vmr, c, flag.Args()[2])
		failError(err)
		log.Println("Keys sent")

	case "nextid":
		id, err := c.GetNextID(context.Background(), 0)
		failError(err)
		log.Printf("Getting Next Free ID: %d\n", id)

	case "checkid":
		if len(flag.Args()) < 2 {
			fmt.Printf("Missing vmid\n")
			os.Exit(1)
		}
		i, err := strconv.Atoi(flag.Args()[1])
		failError(err)
		exists, err := c.VMIdExists(context.Background(), i)
		failError(err)
		if exists {
			log.Printf("Selected ID is in use: %d\n", i)
		} else {
			log.Printf("Selected ID is free: %d\n", i)
		}
		// TODO make migrate in new cli
	case "migrate":
		vmr := proxmox.NewVmRef(vmid)
		c.GetVmInfo(context.Background(), vmr)
		args := flag.Args()
		if len(args) <= 1 {
			fmt.Printf("Missing target node\n")
			os.Exit(1)
		}
		_, err := c.MigrateNode(context.Background(), vmr, args[2], true)

		if err != nil {
			log.Printf("Error to move %+v\n", err)
			os.Exit(1)
		}
		log.Printf("VM %d is moved on %s\n", vmid, args[1])

	case "getNodeList":
		nodes, err := c.GetNodeList(context.Background())
		if err != nil {
			log.Printf("Error listing Nodes %+v\n", err)
			os.Exit(1)
		}
		nodeList, err := json.Marshal(nodes)
		failError(err)
		fmt.Println(string(nodeList))

	// only returns enabled resources
	// TODO make getResourceList in new cli
	case "getResourceList":
		resource, err := c.GetResourceList(context.Background(), "")
		if err != nil {
			log.Printf("Error listing resources %+v\n", err)
			os.Exit(1)
		}
		rsList, err := json.Marshal(resource)
		failError(err)
		fmt.Println(string(rsList))

	case "getVmList":
		vms, err := c.GetVmList(context.Background())
		if err != nil {
			log.Printf("Error listing VMs %+v\n", err)
			os.Exit(1)
		}
		vmList, err := json.Marshal(vms)
		failError(err)
		fmt.Println(string(vmList))

	case "getVmInfo":
		if len(flag.Args()) < 2 {
			fmt.Printf("Missing vmid\n")
			os.Exit(1)
		}
		i, err := strconv.Atoi(flag.Args()[1])
		failError(err)
		vmr := proxmox.NewVmRef(i)
		config, err := proxmox.NewConfigQemuFromApi(context.Background(), vmr, c)
		failError(err)
		fmt.Println(config)
		// TODO make getVmInfo in new cli
	case "getVersion":
		versionInfo, err := c.GetVersion(context.Background())
		failError(err)
		version, err := json.Marshal(versionInfo)
		failError(err)
		fmt.Println(string(version))

	//Pool
	case "getPoolList":
		pools, err := c.GetPoolList(context.Background())
		if err != nil {
			log.Printf("Error listing pools %+v\n", err)
			os.Exit(1)
		}
		poolList, err := json.Marshal(pools)
		failError(err)
		fmt.Println(string(poolList))

	case "getPoolInfo":
		if len(flag.Args()) < 2 {
			log.Printf("Error poolid required")
			os.Exit(1)
		}
		poolid := flag.Args()[1]
		poolinfo, err := c.GetPoolInfo(context.Background(), poolid)
		if err != nil {
			log.Printf("Error getting pool info %+v\n", err)
			os.Exit(1)
		}
		poolList, err := json.Marshal(poolinfo)
		failError(err)
		fmt.Println(string(poolList))

	case "createPool":
		if len(flag.Args()) < 2 {
			log.Printf("Error: poolid required")
			os.Exit(1)
		}
		poolid := flag.Args()[1]

		comment := ""
		if len(flag.Args()) == 3 {
			comment = flag.Args()[2]
		}

		err := c.CreatePool(context.Background(), poolid, comment)
		failError(err)
		fmt.Printf("Pool %s created\n", poolid)

	case "deletePool":
		if len(flag.Args()) < 2 {
			log.Printf("Error: poolid required")
			os.Exit(1)
		}
		poolid := flag.Args()[1]

		err := c.DeletePool(context.Background(), poolid)
		failError(err)
		fmt.Printf("Pool %s removed\n", poolid)

	case "updatePoolComment":
		if len(flag.Args()) < 3 {
			log.Printf("Error: poolid and comment required")
			os.Exit(1)
		}

		poolid := flag.Args()[1]
		comment := flag.Args()[2]

		err := c.UpdatePoolComment(context.Background(), poolid, comment)
		failError(err)
		fmt.Printf("Pool %s updated\n", poolid)

	//Users
	case "getUser":
		var config interface{}
		userId, err := proxmox.NewUserID(flag.Args()[1])
		failError(err)
		config, err = proxmox.NewConfigUserFromApi(context.Background(), userId, c)
		failError(err)
		cj, err := json.MarshalIndent(config, "", "  ")
		failError(err)
		log.Println(string(cj))

	case "getUserList":
		users, err := proxmox.ListUsers(context.Background(), c, true)
		if err != nil {
			log.Printf("Error listing users %+v\n", err)
			os.Exit(1)
		}
		userList, err := json.Marshal(users)
		failError(err)
		fmt.Println(string(userList))

	case "updateUserPassword":
		if len(flag.Args()) < 3 {
			log.Printf("Error: Userid and Password required")
			os.Exit(1)
		}
		userId, err := proxmox.NewUserID(flag.Args()[1])
		failError(err)
		err = proxmox.ConfigUser{
			Password: proxmox.UserPassword(flag.Args()[2]),
			User:     userId,
		}.UpdateUserPassword(context.Background(), c)
		failError(err)
		fmt.Printf("Password of User %s updated\n", userId.ToString())

	case "setUser":
		var password proxmox.UserPassword
		config, err := proxmox.NewConfigUserFromJson(GetConfig(*fConfigFile))
		failError(err)
		userId, err := proxmox.NewUserID(flag.Args()[1])
		failError(err)
		if len(flag.Args()) > 2 {
			password = proxmox.UserPassword(flag.Args()[2])
		}
		failError(config.SetUser(context.Background(), userId, password, c))
		log.Printf("User %s has been configured\n", userId.ToString())

	case "deleteUser":
		if len(flag.Args()) < 2 {
			log.Printf("Error: userId required")
			os.Exit(1)
		}
		userId, err := proxmox.NewUserID(flag.Args()[1])
		failError(err)
		err = proxmox.ConfigUser{User: userId}.DeleteUser(context.Background(), c)
		failError(err)
		fmt.Printf("User %s removed\n", userId.ToString())

	//ACME Account
	case "getAcmeAccountList":
		accounts, err := c.GetAcmeAccountList(context.Background())
		if err != nil {
			log.Printf("Error listing Acme accounts %+v\n", err)
			os.Exit(1)
		}
		accountList, err := json.Marshal(accounts)
		failError(err)
		fmt.Println(string(accountList))

	case "getAcmeAccount":
		if len(flag.Args()) < 2 {
			log.Printf("Error: Acme account name required")
			os.Exit(1)
		}
		var config interface{}
		acmeid := flag.Args()[1]
		config, err := proxmox.NewConfigAcmeAccountFromApi(acmeid, c)
		failError(err)
		cj, err := json.MarshalIndent(config, "", "  ")
		failError(err)
		log.Println(string(cj))

	case "createAcmeAccount":
		if len(flag.Args()) < 2 {
			log.Printf("Error: Acme account name required")
			os.Exit(1)
		}
		config, err := proxmox.NewConfigAcmeAccountFromJson(GetConfig(*fConfigFile))
		failError(err)
		acmeid := flag.Args()[1]
		failError(config.CreateAcmeAccount(acmeid, c))
		log.Printf("Acme account %s has been created\n", acmeid)
		// TODO make updateAcmeAccountEmail in new cli
	case "updateAcmeAccountEmail":
		if len(flag.Args()) < 3 {
			log.Printf("Error: acme name and email(s) required")
			os.Exit(1)
		}
		acmeid := flag.Args()[1]
		_, err := c.UpdateAcmeAccountEmails(context.Background(), acmeid, flag.Args()[2])
		failError(err)
		fmt.Printf("Acme account %s has been updated\n", acmeid)

	case "deleteAcmeAccount":
		if len(flag.Args()) < 2 {
			log.Printf("Error: Acme account name required")
			os.Exit(1)
		}
		acmeid := flag.Args()[1]
		_, err := c.DeleteAcmeAccount(context.Background(), acmeid)
		failError(err)
		fmt.Printf("Acme account %s removed\n", acmeid)

	//ACME Plugin
	case "getAcmePluginList":
		plugins, err := c.GetAcmePluginList(context.Background())
		if err != nil {
			log.Printf("Error listing Acme plugins %+v\n", err)
			os.Exit(1)
		}
		pluginList, err := json.Marshal(plugins)
		failError(err)
		fmt.Println(string(pluginList))
		// TODO make getAcmePlugin in new cli
	case "getAcmePlugin":
		var config interface{}
		pluginid := flag.Args()[1]
		config, err := proxmox.NewConfigAcmePluginFromApi(pluginid, c)
		failError(err)
		cj, err := json.MarshalIndent(config, "", "  ")
		failError(err)
		log.Println(string(cj))
		// TODO make setAcmePlugin in new cli
	case "setAcmePlugin":
		if len(flag.Args()) < 2 {
			log.Printf("Error: Acme plugin name required")
			os.Exit(1)
		}
		config, err := proxmox.NewConfigAcmePluginFromJson(GetConfig(*fConfigFile))
		failError(err)
		pluginid := flag.Args()[1]
		failError(config.SetAcmePlugin(pluginid, c))
		log.Printf("Acme plugin %s has been configured\n", pluginid)
		// TODO make deleteAcmePlugin in new cli
	case "deleteAcmePlugin":
		if len(flag.Args()) < 2 {
			log.Printf("Error: Acme plugin name required")
			os.Exit(1)
		}
		pluginid := flag.Args()[1]
		err := c.DeleteAcmePlugin(context.Background(), pluginid)
		failError(err)
		fmt.Printf("Acme plugin %s removed\n", pluginid)

	//Metrics
	case "getMetricsServer":
		var config interface{}
		metricsid := flag.Args()[1]
		config, err := proxmox.NewConfigMetricsFromApi(metricsid, c)
		failError(err)
		cj, err := json.MarshalIndent(config, "", "  ")
		failError(err)
		log.Println(string(cj))

	case "getMetricsServerList":
		metrics, err := c.GetMetricsServerList(context.Background())
		if err != nil {
			log.Printf("Error listing Metrics Servers %+v\n", err)
			os.Exit(1)
		}
		metricList, err := json.Marshal(metrics)
		failError(err)
		fmt.Println(string(metricList))

	case "setMetricsServer":
		config, err := proxmox.NewConfigMetricsFromJson(GetConfig(*fConfigFile))
		failError(err)
		meticsid := flag.Args()[1]
		failError(config.SetMetrics(meticsid, c))
		log.Printf("Merics Server %s has been configured\n", meticsid)

	case "deleteMetricsServer":
		if len(flag.Args()) < 2 {
			log.Printf("Error: Metrics Server name required")
			os.Exit(1)
		}
		metricsid := flag.Args()[1]
		err := c.DeleteMetricServer(context.Background(), metricsid)
		failError(err)
		fmt.Printf("Metrics Server %s removed\n", metricsid)

	//Storage
	case "getStorageList":
		storage, err := c.GetStorageList(context.Background())
		if err != nil {
			log.Printf("Error listing Storages %+v\n", err)
			os.Exit(1)
		}
		storageList, err := json.Marshal(storage)
		failError(err)
		fmt.Println(string(storageList))

	case "getStorage":
		if len(flag.Args()) < 2 {
			log.Printf("Error: Storage id required")
			os.Exit(1)
		}
		var config interface{}
		storageid := flag.Args()[1]
		config, err := proxmox.NewConfigStorageFromApi(context.Background(), storageid, c)
		failError(err)
		cj, err := json.MarshalIndent(config, "", "  ")
		failError(err)
		log.Println(string(cj))

	case "createStorage":
		if len(flag.Args()) < 2 {
			log.Printf("Error: Storage id required")
			os.Exit(1)
		}
		config, err := proxmox.NewConfigStorageFromJson(GetConfig(*fConfigFile))
		failError(err)
		storageid := flag.Args()[1]
		failError(config.CreateWithValidate(context.Background(), storageid, c))
		log.Printf("Storage %s has been created\n", storageid)

	case "updateStorage":
		if len(flag.Args()) < 2 {
			log.Printf("Error: Storage id required")
			os.Exit(1)
		}
		config, err := proxmox.NewConfigStorageFromJson(GetConfig(*fConfigFile))
		failError(err)
		storageid := flag.Args()[1]
		failError(config.UpdateWithValidate(context.Background(), storageid, c))
		log.Printf("Storage %s has been updated\n", storageid)

	case "deleteStorage":
		if len(flag.Args()) < 2 {
			log.Printf("Error: Storage id required")
			os.Exit(1)
		}
		storageid := flag.Args()[1]
		err := c.DeleteStorage(context.Background(), storageid)
		failError(err)
		fmt.Printf("Storage %s removed\n", storageid)

	// Network
	case "getNetworkList":
		if len(flag.Args()) < 2 {
			failError(fmt.Errorf("error: Proxmox node name required"))
		}
		node := flag.Args()[1]
		typeFilter := ""
		if len(flag.Args()) == 3 {
			typeFilter = flag.Args()[2]
		}
		exitStatus, err := c.GetNetworkList(context.Background(), node, typeFilter)
		if err != nil {
			failError(fmt.Errorf("error: %+v\n api error: %s", err, exitStatus))
		}
		log.Printf("List of current network configuration: %s", exitStatus)

	case "getNetworkInterface":
		if len(flag.Args()) < 3 {
			failError(fmt.Errorf("error: Proxmox node name and network interface name required"))
		}
		node := flag.Args()[1]
		iface := flag.Args()[2]
		exitStatus, err := c.GetNetworkInterface(context.Background(), node, iface)
		if err != nil {
			failError(fmt.Errorf("error: %+v\n api error: %s", err, exitStatus))
		}
		log.Printf("Network interface %s configuration: %s", iface, exitStatus)

	case "createNetwork":
		config, err := proxmox.NewConfigNetworkFromJSON(GetConfig(*fConfigFile))
		failError(err)
		failError(config.CreateNetwork(context.Background(), c))
		log.Printf("Network %s has been created\n", config.Iface)

	case "updateNetwork":
		config, err := proxmox.NewConfigNetworkFromJSON(GetConfig(*fConfigFile))
		failError(err)
		failError(config.UpdateNetwork(context.Background(), c))
		log.Printf("Network %s has been updated\n", config.Iface)

	case "deleteNetwork":
		if len(flag.Args()) < 3 {
			failError(fmt.Errorf("error: Proxmox node name and network interface name required"))
		}
		node := flag.Args()[1]
		iface := flag.Args()[2]
		exitStatus, err := c.DeleteNetwork(context.Background(), node, iface)
		if err != nil {
			failError(fmt.Errorf("error: %+v\n api error: %s", err, exitStatus))
		}
		log.Printf("Network interface %s deleted", iface)

	case "applyNetwork":
		if len(flag.Args()) < 2 {
			failError(fmt.Errorf("error: Proxmox node name required"))
		}
		node := flag.Args()[1]
		exitStatus, err := c.ApplyNetwork(context.Background(), node)
		if err != nil {
			failError(fmt.Errorf("error: %+v\n api error: %s", err, exitStatus))
		}
		log.Printf("Network configuration on node %s has been applied\n", node)

	case "revertNetwork":
		if len(flag.Args()) < 2 {
			failError(fmt.Errorf("error: Proxmox node name required"))
		}
		node := flag.Args()[1]
		exitStatus, err := c.RevertNetwork(context.Background(), node)
		if err != nil {
			failError(fmt.Errorf("error: %+v\n api error: %s", err, exitStatus))
		}
		log.Printf("Network configuration on node %s has been reverted\n", node)

	default:
		fmt.Printf("unknown action, try start|stop vmid\n")
	}
	if jbody != nil {
		log.Println(jbody)
	}
	//log.Println(vmr)
}

var rxUserRequiresToken = regexp.MustCompile("[a-z0-9]+@[a-z0-9]+![a-z0-9]+")

func userRequiresAPIToken(userID string) bool {
	return rxUserRequiresToken.MatchString(userID)
}

// GetConfig get config from file
func GetConfig(configFile string) (configSource []byte) {
	var err error
	if configFile != "" {
		configSource, err = os.ReadFile(configFile)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		configSource, err = io.ReadAll(os.Stdin)
		if err != nil {
			log.Fatal(err)
		}
	}
	return
}

func failError(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
