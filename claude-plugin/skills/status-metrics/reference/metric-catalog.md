# Status metric catalog

Every family below also carries `resourcemanager_datumapis_com_project_name`,
`resource_name`, and `resource_namespace`. "Extra labels" are the ones beyond
those. "Conditions" are the `condition` values worth watching; a resource can
publish others.

## Application Load Balancer and Gateway API

| Family | Extra labels | Conditions |
| --- | --- | --- |
| `datum_cloud_networking_http_proxy_info` | `uid` | |
| `datum_cloud_networking_http_proxy_status_condition` | `condition`, `status`, `reason` | `Accepted`, `Programmed` |
| `datum_cloud_networking_http_proxy_custom_hostname` | `hostname` | |
| `datum_cloud_networking_gateway_info` | `uid`, `gatewayclass_name` | |
| `datum_cloud_networking_gateway_status` | `condition`, `status`, `reason` | `Accepted`, `Programmed` |
| `datum_cloud_networking_gateway_custom_hostname` | `hostname` | |
| `datum_cloud_networking_http_route_info` | `uid` | |
| `datum_cloud_networking_http_route_parent_info` | `parent_kind`, `parent_name`, `parent_section_name`, `parent_port` | |
| `datum_cloud_networking_http_route_status_parent_info` | as above, plus `controller_name` | |
| `datum_cloud_networking_trafficprotectionpolicy_info` | `uid` | |
| `datum_cloud_networking_trafficprotectionpolicy_status_ancestor` | ancestor references | |
| `datum_cloud_networking_envoygateway_backend_info` | `uid`, `type`, `fallback` | |
| `datum_cloud_networking_envoygateway_backend_status_condition` | `condition`, `status`, `reason` | |

The Gateway family is `_status`, not `_status_condition`. HTTPProxy, Gateway,
HTTPRoute, TrafficProtectionPolicy, and Backend also publish `_created`, and all
but Backend publish `_deleted` (the deletion timestamp while a delete is in
progress). HTTPRouteFilter, BackendTrafficPolicy, and SecurityPolicy publish
`_info`, `_owner`, and `_created` under
`datum_cloud_networking_envoygateway_<kind>_`.

## Domains and DNS

| Family | Extra labels | Conditions |
| --- | --- | --- |
| `datum_cloud_networking_domain_info` | `uid`, `domain_name` | |
| `datum_cloud_networking_domain_status_condition` | `condition`, `status`, `reason` | `Verified` |
| `datum_cloud_networking_domain_status_next_verification_attempt` | | Unix seconds of the next ownership check |
| `dns_zone_info` | `uid`, `domain_name`, `visibility` | |
| `dns_zone_status_condition` | `condition`, `status`, `reason` | `Accepted`, `Programmed` |
| `dns_record_set_info` | `uid`, `dns_zone_name`, `record_type` | |
| `dns_record_set_status_condition` | `condition`, `status`, `reason` | `Accepted`, `Programmed` |

## Compute

| Family | Extra labels | Conditions |
| --- | --- | --- |
| `datum_cloud_compute_workload_info` | `uid` | |
| `datum_cloud_compute_workload_status_condition` | `condition`, `status`, `reason` | `Available` |
| `datum_cloud_compute_workload_status_replicas` (`_desired`, `_ready`, `_current`, `_updated`) | | Instance counts |
| `datum_cloud_compute_workload_status_deployments` | | Deployments (one per placement and location) |
| `datum_cloud_compute_workload_placement_status_replicas` (`_desired`, `_ready`) | `placement` | Instance counts per placement |
| `datum_cloud_compute_workload_placement_location_info` | `placement`, `location` | Where each placement resolves |
| `datum_cloud_compute_workload_deployment_info` | `uid`, `compute_datumapis_com_workload_uid` | |
| `datum_cloud_compute_workload_deployment_status_condition` | `condition`, `status`, `reason` | `Available` |
| `datum_cloud_compute_workload_deployment_status_replicas` (`_desired`, `_ready`, `_current`, `_updated`) | | Instance counts |
| `datum_cloud_compute_instance_info` | `uid`, `location`, `instance_type`, `runtime_class`, workload and deployment UIDs | |
| `datum_cloud_compute_instance_status_condition` | `condition`, `status`, `reason` | `Ready`, `Programmed` |
| `datum_cloud_compute_instance_network_interface_info` | `network_ip`, `external_ip` | |

The CPU and memory usage series (`datum_compute_instance_cpu_usage_seconds_total`,
`datum_compute_instance_memory_working_set_bytes`) are usage metrics, not status
metrics. They carry `resource_name` (the Instance), `instance_container`, and
`region`, but no workload name: get a workload's Instances with
`datumctl compute instances --workload=WORKLOAD_NAME`, or join on
`datum_cloud_compute_instance_info`.

## Networks (gVPC)

| Family | Extra labels | Conditions |
| --- | --- | --- |
| `datum_cloud_network_info` | `uid` | |
| `datum_cloud_network_status_condition` | `condition`, `status`, `reason` | `Ready` |
| `datum_cloud_network_binding_info` | `uid`, `network_name`, `network_namespace` | |
| `datum_cloud_network_binding_status_condition` | `network_name`, `condition`, `status`, `reason` | |
| `datum_cloud_network_context_info` | `uid` | |
| `datum_cloud_network_context_status_condition` | `condition`, `status`, `reason` | |
| `datum_cloud_network_policy_info` | `uid` | |
| `datum_cloud_network_policy_status_condition` | `condition`, `status`, `reason` | |
| `datum_cloud_subnet_info` | `uid`, `network_name`, `network_namespace` | |
| `datum_cloud_subnet_status_condition` | `network_name`, `condition`, `status`, `reason` | |
| `datum_cloud_subnet_claim_info` | `uid`, `network_name`, `network_namespace` | |
| `datum_cloud_subnet_claim_status_condition` | `network_name`, `condition`, `status`, `reason` | |

## Telemetry

| Family | Extra labels | Conditions |
| --- | --- | --- |
| `datum_cloud_telemetry_export_policy_info` | `uid` | |
| `datum_cloud_telemetry_export_policy_status_condition` | `condition`, `status`, `reason` | `Ready` |
