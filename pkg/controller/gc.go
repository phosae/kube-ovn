package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/scylladb/go-set/strset"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

var lastNoPodLSP = strset.New()

func (c *Controller) gc() error {
	gcFunctions := []func() error{
		c.gcNode,
		c.gcChassis,
		c.gcLogicalSwitch,
		c.gcCustomLogicalRouter,
		c.gcLogicalSwitchPort,
		c.gcLoadBalancer,
		c.gcPortGroup,
		c.gcStaticRoute,
		c.gcVpcNatGateway,
		c.gcLogicalRouterPort,
		c.gcVip,
		c.gcLbSvcPods,
		c.gcVpcDns,
	}
	for _, gcFunc := range gcFunctions {
		if err := gcFunc(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) gcLogicalRouterPort() error {
	klog.Infof("start to gc logical router port")
	vpcs, err := c.vpcsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list vpc, %v", err)
		return err
	}

	exceptPeerPorts := strset.New()
	for _, vpc := range vpcs {
		for _, peer := range vpc.Status.VpcPeerings {
			exceptPeerPorts.Add(fmt.Sprintf("%s-%s", vpc.Name, peer))
		}
	}

	if err = c.ovnClient.DeleteLogicalRouterPorts(nil, logicalRouterPortFilter(exceptPeerPorts)); err != nil {
		klog.Errorf("delete non-existent peer logical router port: %v", err)
		return err
	}
	return nil
}

func (c *Controller) gcVpcNatGateway() error {
	klog.Infof("start to gc vpc nat gateway")
	gws, err := c.vpcNatGatewayLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list vpc nat gateway, %v", err)
		return err
	}

	var gwStsNames []string
	for _, gw := range gws {
		_, err = c.vpcsLister.Get(gw.Spec.Vpc)
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				klog.Errorf("failed to get vpc, %v", err)
				return err
			}
			if err = c.config.KubeOvnClient.KubeovnV1().VpcNatGateways().Delete(context.Background(), gw.Name, metav1.DeleteOptions{}); err != nil {
				klog.Errorf("failed to delete vpc nat gateway, %v", err)
				return err
			}
		}
		gwStsNames = append(gwStsNames, util.GenNatGwStsName(gw.Name))
	}

	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{util.VpcNatGatewayLabel: "true"}})
	stss, err := c.config.KubeClient.AppsV1().StatefulSets(c.config.PodNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: sel.String(),
	})
	if err != nil {
		klog.Errorf("failed to list vpc nat gateway statefulset, %v", err)
		return err
	}
	for _, sts := range stss.Items {
		if !util.ContainsString(gwStsNames, sts.Name) {
			err = c.config.KubeClient.AppsV1().StatefulSets(c.config.PodNamespace).Delete(context.Background(), sts.Name, metav1.DeleteOptions{})
			if err != nil {
				klog.Errorf("failed to delete vpc nat gateway statefulset, %v", err)
				return err
			}
		}
	}
	return nil
}

func (c *Controller) gcLogicalSwitch() error {
	klog.Infof("start to gc logical switch")
	subnets, err := c.subnetsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list subnet, %v", err)
		return err
	}
	subnetNames := strset.NewWithSize(len(subnets))
	subnetMap := make(map[string]*kubeovnv1.Subnet, len(subnets))
	for _, s := range subnets {
		subnetMap[s.Name] = s
		subnetNames.Add(s.Name)
	}

	lss, err := c.ovnClient.ListLogicalSwitch(c.config.EnableExternalVpc, nil)
	if err != nil {
		klog.Errorf("list logical switch: %v", err)
		return err
	}

	klog.Infof("ls in ovn %v", lss)
	klog.Infof("subnet in kubernetes %v", subnetNames)
	for _, ls := range lss {
		if ls.Name == util.InterconnectionSwitch ||
			ls.Name == util.ExternalGatewaySwitch ||
			ls.Name == c.config.ExternalGatewaySwitch {
			continue
		}
		if s := subnetMap[ls.Name]; s != nil && isOvnSubnet(s) {
			continue
		}

		klog.Infof("gc subnet %s", ls)
		if err := c.handleDeleteLogicalSwitch(ls.Name); err != nil {
			klog.Errorf("failed to gc subnet %s, %v", ls, err)
			return err
		}
	}

	klog.Infof("start to gc dhcp options")
	dhcpOptions, err := c.ovnClient.ListDHCPOptions(c.config.EnableExternalVpc, nil)
	if err != nil {
		klog.Errorf("failed to list dhcp options, %v", err)
		return err
	}
	var uuidToDeleteList = []string{}
	for _, item := range dhcpOptions {
		if len(item.ExternalIDs) == 0 || !subnetNames.Has(item.ExternalIDs["ls"]) {
			uuidToDeleteList = append(uuidToDeleteList, item.UUID)
		}
	}
	klog.Infof("gc dhcp options %v", uuidToDeleteList)
	if len(uuidToDeleteList) > 0 {
		if err = c.ovnClient.DeleteDHCPOptionsByUUIDs(uuidToDeleteList...); err != nil {
			klog.Errorf("failed to delete dhcp options by uuids, %v", err)
			return err
		}
	}
	return nil
}

func (c *Controller) gcCustomLogicalRouter() error {
	klog.Infof("start to gc logical router")
	vpcs, err := c.vpcsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list vpc, %v", err)
		return err
	}
	vpcNames := make([]string, 0, len(vpcs))
	for _, s := range vpcs {
		vpcNames = append(vpcNames, s.Name)
	}

	lrs, err := c.ovnClient.ListLogicalRouter(c.config.EnableExternalVpc, nil)
	if err != nil {
		klog.Errorf("failed to list logical router, %v", err)
		return err
	}

	klog.Infof("lr in ovn %v", lrs)
	klog.Infof("vpc in kubernetes %v", vpcNames)

	for _, lr := range lrs {
		if lr.Name == c.config.ClusterRouter {
			continue
		}
		if !util.IsStringIn(lr.Name, vpcNames) {
			klog.Infof("gc router %s", lr)
			if err := c.deleteVpcRouter(lr.Name); err != nil {
				klog.Errorf("failed to delete router %s, %v", lr, err)
				return err
			}
		}
	}
	return nil
}

func (c *Controller) gcNode() error {
	klog.Infof("start to gc nodes")
	nodes, err := c.nodesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list node, %v", err)
		return err
	}
	nodeNames := make([]string, 0, len(nodes))
	for _, no := range nodes {
		nodeNames = append(nodeNames, no.Name)
	}
	ips, err := c.ipsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list ip, %v", err)
		return err
	}
	ipNodeNames := make([]string, 0, len(ips))
	for _, ip := range ips {
		if !strings.Contains(ip.Name, ".") {
			ipNodeNames = append(ipNodeNames, strings.TrimPrefix(ip.Name, "node-"))
		}
	}
	for _, no := range ipNodeNames {
		if !util.IsStringIn(no, nodeNames) {
			klog.Infof("gc node %s", no)
			if err := c.handleDeleteNode(no); err != nil {
				klog.Errorf("failed to gc node %s, %v", no, err)
				return err
			}
		}
	}
	return nil
}

func (c *Controller) gcVip() error {
	klog.Infof("start to gc vips")
	selector, err := util.LabelSelectorNotEmpty(util.IpReservedLabel)
	if err != nil {
		klog.Errorf("failed to generate selector for label %s: %v", util.IpReservedLabel, err)
		return err
	}
	vips, err := c.virtualIpsLister.List(selector)
	if err != nil {
		klog.Errorf("failed to list VIPs: %v", err)
		return err
	}
	for _, vip := range vips {
		portName := vip.Labels[util.IpReservedLabel]
		portNameSplits := strings.Split(portName, ".")
		if len(portNameSplits) >= 2 {
			podName := portNameSplits[0]
			namespace := portNameSplits[1]
			_, err := c.podsLister.Pods(namespace).Get(podName)
			if err != nil {
				if k8serrors.IsNotFound(err) {
					if err = c.releaseVip(vip.Name); err != nil {
						klog.Errorf("failed to clean label from vip %s, %v", vip.Name, err)
						return err
					}
					return nil
				}
				return err
			}
		}
	}
	return nil
}

func (c *Controller) gcLogicalSwitchPort() error {
	klog.Info("start to gc logical switch port")
	if err := c.markAndCleanLSP(); err != nil {
		return err
	}
	return c.markAndCleanLSP()
}

func (c *Controller) markAndCleanLSP() error {
	klog.V(4).Infof("start to gc logical switch ports")
	pods, err := c.podsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list ip, %v", err)
		return err
	}
	nodes, err := c.nodesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list node, %v", err)
		return err
	}
	ipMap := strset.NewWithSize(len(pods) + len(nodes))
	for _, pod := range pods {
		if isStsPod, sts := isStatefulSetPod(pod); isStsPod {
			if isStatefulSetPodToDel(c.config.KubeClient, pod, sts) {
				continue
			}
		} else if !isPodAlive(pod) {
			continue
		}
		podName := c.getNameByPod(pod)

		for k, v := range pod.Annotations {
			if !strings.Contains(k, util.AllocatedAnnotationSuffix) || v != "true" {
				continue
			}
			providerName := strings.ReplaceAll(k, util.AllocatedAnnotationSuffix, "")
			isProviderOvn, err := c.isOVNProvided(providerName, pod)
			if err != nil {
				klog.Errorf("determine if provider is ovn failed %v", err)
			}
			if !isProviderOvn {
				continue
			}
			ipMap.Add(ovs.PodNameToPortName(podName, pod.Namespace, providerName))
		}
	}
	for _, node := range nodes {
		if node.Annotations[util.AllocatedAnnotation] == "true" {
			ipMap.Add(fmt.Sprintf("node-%s", node.Name))
		}

		if _, err := c.ovnEipsLister.Get(node.Name); err == nil {
			// node external gw lsp is managed by ovn eip cr, skip gc its lsp
			ipMap.Add(node.Name)
		}
	}

	// The lsp for vm pod should not be deleted if vm still exists
	ipMap.Add(c.getVmLsps()...)

	lsps, err := c.ovnClient.ListNormalLogicalSwitchPorts(c.config.EnableExternalVpc, nil)
	if err != nil {
		klog.Errorf("failed to list logical switch port, %v", err)
		return err
	}

	noPodLSP := strset.New()
	lspMap := strset.NewWithSize(len(lsps))
	for _, lsp := range lsps {
		lspMap.Add(lsp.Name)
		if ipMap.Has(lsp.Name) {
			continue
		}

		if lsp.Options != nil && lsp.Options["arp_proxy"] == "true" {
			// arp_proxy lsp is a type of vip crd which should not gc
			continue
		}
		if !lastNoPodLSP.Has(lsp.Name) {
			noPodLSP.Add(lsp.Name)
			continue
		}

		klog.Infof("gc logical switch port %s", lsp.Name)
		if err := c.ovnClient.DeleteLogicalSwitchPort(lsp.Name); err != nil {
			klog.Errorf("failed to delete lsp %s: %v", lsp.Name, err)
			return err
		}

		if err := c.config.KubeOvnClient.KubeovnV1().IPs().Delete(context.Background(), lsp.Name, metav1.DeleteOptions{}); err != nil {
			if !k8serrors.IsNotFound(err) {
				klog.Errorf("failed to delete ip %s, %v", lsp.Name, err)
				return err
			}
		}

		if key := lsp.ExternalIDs["pod"]; key != "" {
			c.ipam.ReleaseAddressByPod(key)
		}
	}
	lastNoPodLSP = noPodLSP

	ipMap.Each(func(ipName string) bool {
		if !lspMap.Has(ipName) {
			klog.Errorf("lsp lost for pod %s, please delete the pod and retry", ipName)
		}
		return true
	})

	return nil
}

func (c *Controller) gcLoadBalancer() error {
	klog.Infof("start to gc loadbalancers")
	if !c.config.EnableLb {
		// remove lb from logical switch
		vpcs, err := c.vpcsLister.List(labels.Everything())
		if err != nil {
			return err
		}
		for _, cachedVpc := range vpcs {
			vpc := cachedVpc.DeepCopy()
			for _, subnetName := range vpc.Status.Subnets {
				subnet, err := c.subnetsLister.Get(subnetName)
				if err != nil {
					if k8serrors.IsNotFound(err) {
						continue
					}
					return err
				}
				if !isOvnSubnet(subnet) {
					continue
				}

				lbs := []string{vpc.Status.TcpLoadBalancer, vpc.Status.TcpSessionLoadBalancer, vpc.Status.UdpLoadBalancer, vpc.Status.UdpSessionLoadBalancer, vpc.Status.SctpLoadBalancer, vpc.Status.SctpSessionLoadBalancer}
				if err := c.ovnClient.LogicalSwitchUpdateLoadBalancers(subnetName, ovsdb.MutateOperationDelete, lbs...); err != nil {
					return err
				}
			}

			vpc.Status.TcpLoadBalancer = ""
			vpc.Status.TcpSessionLoadBalancer = ""
			vpc.Status.UdpLoadBalancer = ""
			vpc.Status.UdpSessionLoadBalancer = ""
			vpc.Status.SctpLoadBalancer = ""
			vpc.Status.SctpSessionLoadBalancer = ""
			bytes, err := vpc.Status.Bytes()
			if err != nil {
				return err
			}
			_, err = c.config.KubeOvnClient.KubeovnV1().Vpcs().Patch(context.Background(), vpc.Name, types.MergePatchType, bytes, metav1.PatchOptions{}, "status")
			if err != nil {
				return err
			}
		}

		// lbs will remove from logical switch automatically when delete lbs
		if err = c.ovnClient.DeleteLoadBalancers(nil); err != nil {
			klog.Errorf("delete all load balancers: %v", err)
			return err
		}
		return nil
	}

	svcs, err := c.servicesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list svc, %v", err)
		return err
	}
	tcpVips := strset.NewWithSize(len(svcs) * 2)
	udpVips := strset.NewWithSize(len(svcs) * 2)
	sctpVips := strset.NewWithSize(len(svcs) * 2)
	tcpSessionVips := strset.NewWithSize(len(svcs) * 2)
	udpSessionVips := strset.NewWithSize(len(svcs) * 2)
	sctpSessionVips := strset.NewWithSize(len(svcs) * 2)
	for _, svc := range svcs {
		ips := util.ServiceClusterIPs(*svc)
		if v, ok := svc.Annotations[util.SwitchLBRuleVipsAnnotation]; ok {
			ips = strings.Split(v, ",")
		}

		for _, ip := range ips {
			for _, port := range svc.Spec.Ports {
				vip := util.JoinHostPort(ip, port.Port)
				switch port.Protocol {
				case corev1.ProtocolTCP:
					if svc.Spec.SessionAffinity == corev1.ServiceAffinityClientIP {
						tcpSessionVips.Add(vip)
					} else {
						tcpVips.Add(vip)
					}
				case corev1.ProtocolUDP:
					if svc.Spec.SessionAffinity == corev1.ServiceAffinityClientIP {
						udpSessionVips.Add(vip)
					} else {
						udpVips.Add(vip)
					}
				case corev1.ProtocolSCTP:
					if svc.Spec.SessionAffinity == corev1.ServiceAffinityClientIP {
						sctpSessionVips.Add(vip)
					} else {
						sctpVips.Add(vip)
					}
				}
			}
		}
	}

	vpcs, err := c.vpcsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list vpc, %v", err)
		return err
	}
	var vpcLbs []string
	for _, vpc := range vpcs {
		tcpLb, udpLb, sctpLb := vpc.Status.TcpLoadBalancer, vpc.Status.UdpLoadBalancer, vpc.Status.SctpLoadBalancer
		tcpSessLb, udpSessLb, sctpSessLb := vpc.Status.TcpSessionLoadBalancer, vpc.Status.UdpSessionLoadBalancer, vpc.Status.SctpSessionLoadBalancer
		vpcLbs = append(vpcLbs, tcpLb, udpLb, sctpLb, tcpSessLb, udpSessLb, sctpSessLb)

		removeVIP := func(lbName string, svcVips *strset.Set) error {
			if lbName == "" {
				return nil
			}

			lb, err := c.ovnClient.GetLoadBalancer(lbName, true)
			if err != nil {
				klog.Errorf("get LB %s: %v", lbName, err)
				return err
			}
			if lb == nil {
				klog.Infof("load balancer %q not found", lbName)
				return nil
			}

			for vip := range lb.Vips {
				if !svcVips.Has(vip) {
					if err = c.ovnClient.LoadBalancerDeleteVip(lbName, vip); err != nil {
						klog.Errorf("failed to delete vip %s from LB %s: %v", vip, lbName, err)
						return err
					}
				}
			}
			return nil
		}

		if err = removeVIP(tcpLb, tcpVips); err != nil {
			return err
		}
		if err = removeVIP(tcpSessLb, tcpSessionVips); err != nil {
			return err
		}
		if err = removeVIP(udpLb, udpVips); err != nil {
			return err
		}
		if err = removeVIP(udpSessLb, udpSessionVips); err != nil {
			return err
		}
		if err = removeVIP(sctpLb, sctpVips); err != nil {
			return err
		}
		if err = removeVIP(sctpSessLb, sctpSessionVips); err != nil {
			return err
		}
	}

	// delete lbs
	if err = c.ovnClient.DeleteLoadBalancers(func(lb *ovnnb.LoadBalancer) bool {
		return !util.ContainsString(vpcLbs, lb.Name)
	}); err != nil {
		klog.Errorf("delete load balancers: %v", err)
		return err
	}

	return nil
}

func (c *Controller) gcPortGroup() error {
	klog.Infof("start to gc network policy")

	npNames := strset.New()

	if c.config.EnableNP {
		nps, err := c.npsLister.List(labels.Everything())
		if err != nil {
			klog.Errorf("failed to list network policy, %v", err)
			return err
		}

		for _, np := range nps {
			npNames.Add(fmt.Sprintf("%s/%s", np.Namespace, np.Name))
		}

		// append node port group to npNames to avoid gc node port group
		nodes, err := c.nodesLister.List(labels.Everything())
		if err != nil {
			klog.Errorf("failed to list nodes, %v", err)
			return err
		}

		for _, node := range nodes {
			npNames.Add(fmt.Sprintf("%s/%s", "node", node.Name))
		}

		// append overlay subnets port group to npNames to avoid gc distributed subnets port group
		subnets, err := c.subnetsLister.List(labels.Everything())
		if err != nil {
			klog.Errorf("failed to list subnets %v", err)
			return err
		}
		for _, subnet := range subnets {
			if subnet.Spec.Vpc != c.config.ClusterRouter || (subnet.Spec.Vlan != "" && !subnet.Spec.LogicalGateway) || subnet.Name == c.config.NodeSwitch || subnet.Spec.GatewayType != kubeovnv1.GWDistributedType {
				continue
			}

			for _, node := range nodes {
				npNames.Add(fmt.Sprintf("%s/%s", subnet.Name, node.Name))
			}
		}

		// list all np port groups which externalIDs[np]!=""
		pgs, err := c.ovnClient.ListPortGroups(map[string]string{networkPolicyKey: ""})
		if err != nil {
			klog.Errorf("list np port group: %v", err)
			return err
		}

		for _, pg := range pgs {
			np := strings.Split(pg.ExternalIDs[networkPolicyKey], "/")
			if len(np) != 2 {
				// not np port group
				continue
			}
			if !npNames.Has(pg.ExternalIDs[networkPolicyKey]) {
				klog.Infof("gc port group '%s' network policy '%s'", pg.Name, pg.ExternalIDs[networkPolicyKey])
				c.deleteNpQueue.Add(pg.ExternalIDs[networkPolicyKey])
			}
		}
	}

	return nil
}

func (c *Controller) gcStaticRoute() error {
	klog.Infof("start to gc static routes")
	routes, err := c.ovnClient.ListLogicalRouterStaticRoutes(c.config.ClusterRouter, nil, nil, "", nil)
	if err != nil {
		klog.Errorf("failed to list static route %v", err)
		return err
	}
	defaultVpc, err := c.vpcsLister.Get(c.config.ClusterRouter)
	if err != nil {
		klog.Errorf("failed to get default vpc, %v", err)
		return err
	}
	var keepStaticRoute bool
	for _, route := range routes {
		keepStaticRoute = false
		for _, item := range defaultVpc.Spec.StaticRoutes {
			if route.IPPrefix == item.CIDR && route.Nexthop == item.NextHopIP && route.RouteTable == item.RouteTable {
				keepStaticRoute = true
				break
			}
		}
		if keepStaticRoute {
			continue
		}
		if route.IPPrefix != "0.0.0.0/0" && route.IPPrefix != "::/0" && c.ipam.ContainAddress(route.IPPrefix) {
			exist, err := c.ovnClient.NatExists(c.config.ClusterRouter, "", "", route.IPPrefix)
			if err != nil {
				klog.Errorf("failed to get NatRule by LogicalIP %s, %v", route.IPPrefix, err)
				continue
			}
			if exist {
				continue
			}
			klog.Infof("gc static route %s %v %s %s", route.RouteTable, route.Policy, route.IPPrefix, route.Nexthop)
			if err = c.ovnClient.DeleteLogicalRouterStaticRoute(c.config.ClusterRouter, &route.RouteTable, route.Policy, route.IPPrefix, route.Nexthop); err != nil {
				klog.Errorf("failed to delete stale route %s %v %s %s: %v", route.RouteTable, route.Policy, route.IPPrefix, route.Nexthop, err)
			}
		}
	}
	return nil
}

func (c *Controller) gcChassis() error {
	klog.Infof("start to gc chassis")
	chassises, err := c.ovnLegacyClient.GetAllChassis()
	if err != nil {
		klog.Errorf("failed to get all chassis, %v", err)
	}
	nodes, err := c.nodesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list nodes, %v", err)
		return err
	}
	for _, chassis := range chassises {
		matched := true
		for _, node := range nodes {
			if chassis == node.Annotations[util.ChassisAnnotation] {
				matched = false
				break
			}
		}
		if matched {
			if err := c.ovnLegacyClient.DeleteChassisByName(chassis); err != nil {
				klog.Errorf("failed to delete chassis %s %v", chassis, err)
				return err
			}
		}
	}
	return nil
}

func (c *Controller) isOVNProvided(providerName string, pod *corev1.Pod) (bool, error) {
	if ls, ok := pod.Annotations[fmt.Sprintf(util.LogicalSwitchAnnotationTemplate, providerName)]; ok {
		subnet, err := c.subnetsLister.Get(ls)
		if err != nil {
			klog.Errorf("parse annotation logical switch %s error %v", ls, err)
			return false, err
		}
		if !strings.HasSuffix(subnet.Spec.Provider, util.OvnProvider) {
			return false, nil
		}
		return true, nil
	}
	return false, nil
}

func (c *Controller) getVmLsps() []string {
	var vmLsps []string

	if !c.config.EnableKeepVmIP {
		return vmLsps
	}

	nss, err := c.namespacesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list namespaces, %v", err)
		return vmLsps
	}

	for _, ns := range nss {
		vms, err := c.config.KubevirtClient.VirtualMachine(ns.Name).List(&metav1.ListOptions{})
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				klog.Errorf("failed to list vm in namespace %s, %v", ns, err)
			}
			continue
		} else {
			for _, vm := range vms.Items {
				vmLsp := ovs.PodNameToPortName(vm.Name, ns.Name, util.OvnProvider)
				vmLsps = append(vmLsps, vmLsp)

				attachNets, err := util.ParsePodNetworkAnnotation(vm.Spec.Template.ObjectMeta.Annotations[util.AttachmentNetworkAnnotation], vm.Namespace)
				if err != nil {
					klog.Errorf("failed to get attachment subnet of vm %s, %v", vm.Name, err)
					continue
				}
				for _, multiNet := range attachNets {
					provider := fmt.Sprintf("%s.%s.ovn", multiNet.Name, multiNet.Namespace)
					vmLsp := ovs.PodNameToPortName(vm.Name, ns.Name, provider)
					vmLsps = append(vmLsps, vmLsp)
				}

				for _, network := range vm.Spec.Template.Spec.Networks {
					if network.Multus != nil && network.Multus.NetworkName != "" {
						items := strings.Split(network.Multus.NetworkName, "/")
						if len(items) != 2 {
							continue
						}
						provider := fmt.Sprintf("%s.%s.ovn", items[1], items[0])
						vmLsp := ovs.PodNameToPortName(vm.Name, ns.Name, provider)
						vmLsps = append(vmLsps, vmLsp)
					}
				}

			}
		}
	}

	return vmLsps
}

func (c *Controller) gcLbSvcPods() error {
	klog.Infof("start to gc lb svc pods")
	nss, err := c.namespacesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list namespaces, %v", err)
		return err
	}

	for _, ns := range nss {
		dps, err := c.config.KubeClient.AppsV1().Deployments(ns.Name).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				klog.Errorf("failed to list lb svc deployment in namespace %s, %v", ns.Name, err)
			}
			continue
		}

		for _, dp := range dps.Items {
			if !strings.HasPrefix(dp.Name, "lb-svc-") {
				continue
			}
			if _, ok := dp.Spec.Template.Labels["service"]; !ok {
				continue
			}

			svcName := strings.TrimPrefix(dp.Name, "lb-svc-")
			_, err := c.servicesLister.Services(ns.Name).Get(svcName)
			if err != nil && k8serrors.IsNotFound(err) {
				klog.Infof("gc lb svc deployment %s in ns %s", dp.Name, ns.Name)
				if err := c.config.KubeClient.AppsV1().Deployments(ns.Name).Delete(context.Background(), dp.Name, metav1.DeleteOptions{}); err != nil {
					if !k8serrors.IsNotFound(err) {
						klog.Errorf("failed to delete lb svc deployment in namespace %s, %v", ns.Name, err)
					}
				}
			}
		}
	}
	return nil
}

func (c *Controller) gcVpcDns() error {
	if !c.config.EnableLb {
		return nil
	}

	klog.Infof("start to gc vpc dns")
	vds, err := c.vpcDnsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list vpc-dns, %v", err)
		return err
	}

	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{util.VpcDnsNameLabel: "true"}})

	deps, err := c.config.KubeClient.AppsV1().Deployments(c.config.PodNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: sel.String(),
	})
	if err != nil {
		klog.Errorf("failed to list vpc-dns deployment, %s", err)
		return err
	}

	for _, dep := range deps.Items {
		canFind := false
		for _, vd := range vds {
			name := genVpcDnsDpName(vd.Name)
			if dep.Name == name {
				canFind = true
				break
			}
		}
		if !canFind {
			err := c.config.KubeClient.AppsV1().Deployments(c.config.PodNamespace).Delete(context.Background(),
				dep.Name, metav1.DeleteOptions{})
			if err != nil {
				klog.Errorf("failed to delete vpc-dns deployment, %s", err)
				return err
			}
		}
	}

	slrs, err := c.switchLBRuleLister.List(sel)
	if err != nil {
		klog.Errorf("failed to list vpc-dns SwitchLBRules, %s", err)
		return err
	}

	for _, slr := range slrs {
		canFind := false
		for _, vd := range vds {
			name := genVpcDnsDpName(vd.Name)
			if slr.Name == name {
				canFind = true
				break
			}
		}
		if !canFind {
			err := c.config.KubeOvnClient.KubeovnV1().SwitchLBRules().Delete(context.Background(),
				slr.Name, metav1.DeleteOptions{})
			if err != nil {
				klog.Errorf("failed to delete vpc-dns SwitchLBRule, %s", err)
				return err
			}
		}
	}
	return nil
}

func logicalRouterPortFilter(exceptPeerPorts *strset.Set) func(lrp *ovnnb.LogicalRouterPort) bool {
	return func(lrp *ovnnb.LogicalRouterPort) bool {
		if exceptPeerPorts.Has(lrp.Name) {
			return false // ignore except lrp
		}

		return lrp.Peer != nil && len(*lrp.Peer) != 0
	}
}
