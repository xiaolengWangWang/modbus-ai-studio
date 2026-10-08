package ui

import "modbus-ai-studio/internal/modbus"

// Late bytes can be drained while another request is active. Their recorded
// request fields do not prove ownership; leave these frames unassociated.
func relatedAIPackets(selected modbus.Packet, packets []modbus.Packet) []modbus.Packet {
	if selected.ConnectionID == "" || selected.RequestID == 0 || selected.Status == modbus.StatusLate {
		return nil
	}
	var related []modbus.Packet
	for _, p := range packets {
		if p.ConnectionID != selected.ConnectionID || p.RequestID != selected.RequestID || p.Status == modbus.StatusLate || p.Dir == selected.Dir ||
			p.Mode != selected.Mode || p.Slave != selected.Slave || p.Function != selected.Function || p.Address != selected.Address || p.Count != selected.Count {
			continue
		}
		related = append(related, *cloneAIPacket(&p))
		if len(related) == 64 {
			break
		}
	}
	return related
}

func (ws *Workspace) freezeAITarget(target aiTarget, origin *inspector) aiTarget {
	target = cloneAITarget(target)
	if target.packet == nil {
		return target
	}
	var packets []modbus.Packet
	if origin != nil && origin.selectionOnly {
		packets = origin.packetContext
	} else {
		ws.ring.mu.Lock()
		for i := max(0, ws.ring.n-len(ws.ring.buf)); i < ws.ring.n; i++ {
			packets = append(packets, ws.ring.buf[i%len(ws.ring.buf)])
		}
		ws.ring.mu.Unlock()
	}
	target.related = relatedAIPackets(*target.packet, packets)
	return target
}
