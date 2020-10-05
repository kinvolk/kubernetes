// TODO(Mauricio): What copyright header to use?

package idtools

import (
	"fmt"
)

type IDMapping struct {
	HostID      uint32
	ContainerID uint32
	Size        uint32
}

type IDMappings struct {
	uids []IDMapping
	gids []IDMapping
}

func NewIDMappings(uids, gids []IDMapping) (*IDMappings, error) {
	if !isValid(uids) {
		return nil, fmt.Errorf("UIDs mapping overlap")
	}
	if !isValid(gids) {
		return nil, fmt.Errorf("GIDs mapping overlap")
	}
	return &IDMappings{uids: uids, gids: gids}, nil
}

func isValid(maps []IDMapping) bool {
	if maps == nil {
		return true
	}
	// TODO: maps must be ordered
	// Check overlapping in the container side
	for i := 0; i < len(maps) - 1; i++ {
		if (maps[i].ContainerID + maps[i].Size) > maps[i + 1].ContainerID {
			return false
		}
	}
	// TODO: Check also on the host one
	return true
}

func (id *IDMappings) UIDs() []IDMapping {
	return id.uids
}

func (id *IDMappings) GIDs() []IDMapping {
	return id.gids
}

func (id *IDMappings) UIDToHost(uid uint32) (uint32, error) {
	return toHost(uid, id.uids)
}

func (id *IDMappings) GIDToHost(gid uint32) (uint32, error) {
	return toHost(gid, id.gids)
}

func toHost(contID uint32, maps []IDMapping) (uint32, error) {
	if maps == nil {
		return contID, nil
	}

	for _, m := range maps {
		if contID >= m.ContainerID && (contID < (m.ContainerID + m.Size)) {
			hostID := m.HostID + (contID - m.ContainerID)
			return hostID, nil
		}
	}
	return 0, fmt.Errorf("Container ID %d cannot be mapped to a host ID", contID)
}

func PunchHole(id uint32, maps []IDMapping) []IDMapping {
	if maps == nil {
		return nil
	}

	var tempMaps []IDMapping

	for _, m := range maps {
		// Split this range in two
		if id >= m.ContainerID && (id <= (m.ContainerID + m.Size - 1)) {
			x1 := IDMapping{
				HostID: m.HostID,
				ContainerID: m.ContainerID,
				Size: id - m.ContainerID,
			}
			x2 := IDMapping{
				HostID: m.HostID + (id - m.ContainerID) + 1,
				ContainerID: id + 1,
				Size: m.Size - x1.Size - 1,
			}
			if x1.Size > 0 {
				tempMaps = append(tempMaps, x1)
			}
			if x2.Size > 0 {
				tempMaps = append(tempMaps, x2)
			}
			continue
		}

		tempMaps = append(tempMaps, m)
	}

	// add the 1-to-1 mapping
	return append(tempMaps, IDMapping{id, id, 1})
}
