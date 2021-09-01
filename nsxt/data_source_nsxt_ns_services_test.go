/* Copyright © 2017 VMware, Inc. All Rights Reserved.
   SPDX-License-Identifier: MPL-2.0 */

package nsxt

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccDataSourceNsxtNsServices_basic(t *testing.T) {
	serviceName := getAccTestDataSourceName()
	testResourceName := "data.nsxt_ns_services.test"
	// in order to verify correct functionality, we compare fetching by name with regular data source
	// and fetching using the map data source with same name
	// in order to verify map fetching with pair comparison, we use an extra resource
	checkDataSourceName := "data.nsxt_ns_service.check"
	checkResourceName := "nsxt_ns_group.check"

	resource.ParallelTest(t, resource.TestCase{
		PreCheck:  func() { testAccOnlyLocalManager(t); testAccTestMP(t); testAccPreCheck(t) },
		Providers: testAccProviders,
		CheckDestroy: func(state *terraform.State) error {
			return testAccDataSourceNsxtNsServiceDeleteByName(serviceName)
		},
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					if err := testAccDataSourceNsxtNsServiceCreate(serviceName); err != nil {
						panic(err)
					}
				},
				Config: testAccNSXNsServicesReadTemplate(serviceName),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrSet(testResourceName, "id"),
					resource.TestCheckResourceAttrPair(checkResourceName, "display_name", checkDataSourceName, "id"),
				),
			},
		},
	})
}

func testAccNSXNsServicesReadTemplate(serviceName string) string {
	return fmt.Sprintf(`
data "nsxt_ns_services" "test" {
}

data "nsxt_ns_service" "check" {
  display_name = "%s"
}

resource "nsxt_ns_group" "check" {
  display_name = data.nsxt_ns_services.test.items["%s"]
}`, serviceName, serviceName)
}
