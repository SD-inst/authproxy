package events

type packetType string

const (
	PROGRESS_UPDATE packetType = "progress"
	USERS_UPDATE    packetType = "users"
	GPU_UPDATE      packetType = "gpu"
	DOWNLOAD_UPDATE packetType = "download"
	MESSAGE_UPDATE  packetType = "message"
	SERVICE_UPDATE  packetType = "service"
)

type Packet struct {
	Type      packetType `json:"type"`
	Ephemeral bool       `json:"ephemeral"`
	Data      any        `json:"data"`
}

type MessageUpdate struct {
	Message   string `json:"message"`
	Type      string `json:"type"`
	Subsystem string `json:"subsystem"`
}

type UsersUpdate struct {
	Users    int `json:"users"`
	Sessions int `json:"sessions"`
}

type ServiceUpdate struct {
	Service int `json:"service"`
}
