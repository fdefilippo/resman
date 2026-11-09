package blockiops

// Source exposes complete cumulative partitions and explicit source integrity.
// Callers must confirm systemd authority before registering a unit lifetime.
type Source interface {
	Register(uint32, string, uint64) (Identity, error)
	Read(Identity, []Device) (Sample, error)
	Health() error
	Retain([]Identity) error
	Close() error
}
