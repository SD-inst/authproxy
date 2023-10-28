package events

type packetType string

const (
	PROGRESS_UPDATE packetType = "progress"
	USERS_UPDATE    packetType = "users"
	GPU_UPDATE      packetType = "gpu"
)

type Packet struct {
	Type packetType `json:"type"`
	Data any        `json:"data"`
}

type UsersUpdate struct {
	Users    int `json:"users"`
	Sessions int `json:"sessions"`
}
