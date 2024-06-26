# Kubevirt API Fuzzer

Kubevit api fuzzer is a demo tool to show power of adding unit test for round tripping API objects for compatibility checks
across versions.

### Tools Description:

1. This tool creates JSON and YAML files for all the API exposed by kubevirt in group-version "kubevirt.io/v1", 
   versioned by the release. The current version is in `HEAD` directory, previous versions are in `release-0.yy` release
   directory. APIs includes, more APIs can be added in the future:
    ```
    VirtualMachineInstance
    VirtualMachineInstanceList
    VirtualMachineInstanceReplicaSet
    VirtualMachineInstanceReplicaSetList
    VirtualMachineInstancePreset
    VirtualMachineInstancePresetList
    VirtualMachineInstanceMigration
    VirtualMachineInstanceMigrationList
    VirtualMachine
    VirtualMachineList
    KubeVirt
    KubeVirtList
    ```
2. Upon upgrade to API, the json and YAML files will be updated.
3. When KubeVirt cuts a new release of the project, the current version files will be copied to the release version and
   future development branch will add a unit test for past two releases:
    ```
    $ VERSION=release-0.60
    $ cp -fr testdata/{HEAD,${VERSION}} 
    ```

### Usage:
This demo assumes that upstream kubevirt has been upgraded from 0.48, 0.50 and the current version is 0.59.

To check if the current API(0.59) supports previous versions(0.50 or 0.48), run the following command:
```
OLD_VERSION=release-0.50
go test ./ -run //${OLD_VERSION}
```

Example output:

```    
--- FAIL: TestCompatibility/kubevirt.io.v1.VirtualMachineInstance (0.01s)
        --- FAIL: TestCompatibility/kubevirt.io.v1.VirtualMachineInstance/release-0.50 (0.01s)
            compatibility.go:416: json differs
            compatibility.go:417:   (
                        """
                        ... // 215 identical lines
                                      "readonly": true
                                    },
                -                   "floppy": {
                -                     "readonly": true,
                -                     "tray": "trayValue"
                -                   },
                                    "cdrom": {
                                      "bus": "busValue",
                        ... // 678 identical lines
                              "tscFrequency": -12
                            },
                -           "virtualMachineRevisionName": "virtualMachineRevisionNameValue"
                +           "virtualMachineRevisionName": "virtualMachineRevisionNameValue",
                +           "runtimeUser": 0
                          }
                        }
                        """
                  )
                
            compatibility.go:422: yaml differs
            compatibility.go:423:   (
                        """
                        ... // 237 identical lines
                                  pciAddress: pciAddressValue
                                  readonly: true
                -               floppy:
                -                 readonly: true
                -                 tray: trayValue
                                io: ioValue
                                lun:
                        ... // 341 identical lines
                          qosClass: qosClassValue
                          reason: reasonValue
                +         runtimeUser: 0
                          topologyHints:
                            tscFrequency: -12
                        ... // 22 identical lines
                        """
                  )
                
```

The above output shows that for VirtualMachineInstance:
1. api-field: `spec.domain.devices.disks.floppy` was dropped. [ref-1](https://github.com/kubevirt/kubevirt/issues/2016)[ref-2](https://github.com/kubevirt/kubevirt/pull/2164)
2. api-field: `status.runtimeUser` field was added[ref-3](https://github.com/kubevirt/kubevirt/pull/6709)

The first api-field was intentional hence it can be ignored), the second seems like it was a human error. Upon 
identifying the error using this tool, a fix was pushed via [this commit](https://github.com/alaypatel07/kubevirt-api-fuzzer/commit/0f7febd95f4deb33819d60992d5894a283b3a3bf)
This demonstrates the usability of this tool in automation and during the upgrade process during downstream testing.

Using this:
1. API reviewers can say if changes in current version will break older clients upon upgrade
2. During upgrades, vendors can check the API changes going into the upgrade using simple differ and get a better
   synopsis of what is failing during upgrade.

Next Steps:

1. Ways to Update TestData: 
    1. Version Control: When a new version of KubeVirt is released, it is essential to update the test data to reflect changes and new features introduced in that version.The test data should include scenarios that cover upto the last three versions(n-3) of KubeVirt, allowing for backward compatibility testing and ensuring that the new version does not break functionalities in the older versions.

         1. Remove the Oldest Version TestData :  To manage the scope of the test data effectively and avoid an overload of legacy test scenarios, the oldest version of the test data should be removed as new versions are added. 
         2. Addition of New TestData (Latest Version): With the release of a new version, new test data specific to this version should be added.

    2. On Demand Update: Whenever changes to any components identified, developers or testers can review the current test cases and the associated test data if they will still be valid for the changes or else update the testdata.

    3. Come up with data generation techniques providing invalid, unexpected, or random data as inputs. (least priority)

2. Integrate these unit tests to the CI/CD pipelines.