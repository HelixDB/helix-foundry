package model

import "fmt"

func ExampleFoundryWorkflowRun_Recording() {
	r := &FoundryWorkflowRun{ID: "01RUN", AutomationKey: "k3y"}
	fmt.Println(r.Recording())
	// Output: 01RUN_k3y
}
