package conf

import (
	"fmt"
	"net"
)

func parseIpCidrList(in []string) ([]interface{}, error) {
	if len(in) == 0 {
		return nil, nil
	}

	var ret []interface{}
	for _, t := range in {
		_, ipnet, err := net.ParseCIDR(t)
		if err == nil {
			ret = append(ret, ipnet)
			continue
		}

		ip := net.ParseIP(t)
		if ip != nil {
			ret = append(ret, ip)
			continue
		}

		return nil, fmt.Errorf("unable to parse ip/network '%s'", t)
	}
	return ret, nil
}
