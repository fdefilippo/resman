// Kernel accounting and deliberately bounded direct-I/O provenance. These
// programs do not infer buffered-writeback ownership from the current task.
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>

#define CORE __attribute__((preserve_access_index))
struct kernfs_node { __u64 id; } CORE;
struct cgroup;
struct cgroup_subsys_state { struct cgroup *cgroup; struct cgroup_subsys_state *parent; } CORE;
struct cgroup { struct kernfs_node *kn; struct cgroup_subsys_state self; } CORE;
struct blkcg { struct cgroup_subsys_state css; } CORE;
struct blkcg_gq { struct blkcg *blkcg; } CORE;
struct gendisk { int major; int first_minor; } CORE;
struct block_device { struct gendisk *bd_disk; } CORE;
struct bio { struct blkcg_gq *bi_blkg; struct block_device *bi_bdev; unsigned int bi_opf; } CORE;
struct qstr { const unsigned char *name; unsigned int len; } CORE;
struct dentry_operations { void *d_dname; } CORE;
struct dentry { struct qstr d_name; const struct dentry_operations *d_op; } CORE;
struct path { struct dentry *dentry; } CORE;
struct file { struct path f_path; } CORE;
struct mm_struct { struct file *exe_file; } CORE;
struct task_struct { struct mm_struct *mm; unsigned int flags; } CORE;

struct frame {
	__u64 bio;
	__u64 epoch;
	char executable[256];
};

struct alias_key {
	__u64 task;
	__u64 bio;
};

struct counter_key {
	__u64 cgroup;
	__u64 lifetime;
	__u32 major;
	__u32 minor;
	__u32 direction;
	__u32 reserved;
	char executable[256];
};

// A new lifetime cookie prevents a recreated unit from inheriting old counts.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 4096);
	__type(key, __u64);
	__type(value, __u64);
} watched SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_HASH);
	__uint(max_entries, 32768);
	__type(key, struct counter_key);
	__type(value, __u64);
} operations SEC(".maps");

// Eviction loses attribution, not operation coverage: the operation becomes
// unresolved. Cookies are scoped to a live direct-submit frame, not BIO reuse.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 4096);
	__type(key, __u64);
	__type(value, struct frame);
} frames SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 32768);
	__type(key, struct alias_key);
	__type(value, __u64);
} aliases SEC(".maps");

enum health_index { HEALTH_SEQUENCE, HEALTH_READ_FAILURE, HEALTH_DEPTH, HEALTH_MAP_FULL, HEALTH_OVERFLOW, HEALTH_COUNT };
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, HEALTH_COUNT);
	__type(key, __u32);
	__type(value, __u64);
} health SEC(".maps");

static __always_inline void fail(__u32 reason)
{
	__u64 *value = bpf_map_lookup_elem(&health, &reason);
	if (value)
		__sync_fetch_and_add(value, 1);
}

SEC("fentry/iomap_dio_submit_bio")
int capture_direct(__u64 *ctx)
{
	__u64 task = bpf_get_current_pid_tgid();
	struct frame frame = { .bio = ctx[2] };
	__u32 sequence_key = HEALTH_SEQUENCE;
	__u64 *sequence = bpf_map_lookup_elem(&health, &sequence_key);
	if (!sequence)
		return 0;
	frame.epoch = __sync_fetch_and_add(sequence, 1) + 1;
	if (!frame.epoch) {
		fail(HEALTH_OVERFLOW);
		return 0;
	}
	struct task_struct *current = (void *)bpf_get_current_task();
	unsigned int flags = 0;
	// PF_IO_WORKER and PF_KTHREAD are checked against the supported kernel
	// profile. A worker's executable or credentials do not prove the issuer.
	if (!bpf_core_read(&flags, sizeof(flags), &current->flags) && !(flags & 0x00200010)) {
		struct dentry *entry = BPF_CORE_READ(current, mm, exe_file, f_path.dentry);
		const struct dentry_operations *ops = 0;
		void *dynamic_name = (void *)1;
		if (entry && !bpf_core_read(&ops, sizeof(ops), &entry->d_op)) {
			dynamic_name = 0;
			if (ops && bpf_core_read(&dynamic_name, sizeof(dynamic_name), &ops->d_dname))
				dynamic_name = (void *)1;
		}
		unsigned int length = BPF_CORE_READ(entry, d_name.len);
		const unsigned char *name = BPF_CORE_READ(entry, d_name.name);
		// Custom d_dname renderers need a separate provenance proof. A
		// truncated basename must never be accepted as an executable identity.
		if (!dynamic_name && name && length > 0 && length < sizeof(frame.executable)) {
			long copied = bpf_probe_read_kernel_str(frame.executable, sizeof(frame.executable), name);
			if (copied != length + 1)
				frame.executable[0] = 0;
		}
	}
	bpf_map_update_elem(&frames, &task, &frame, BPF_ANY);
	return 0;
}

SEC("fexit/iomap_dio_submit_bio")
int finish_direct(__u64 *ctx)
{
	__u64 task = bpf_get_current_pid_tgid();
	bpf_map_delete_elem(&frames, &task);
	return 0;
}

SEC("fexit/bio_alloc_clone")
int clone_direct(__u64 *ctx)
{
	struct alias_key key = { .task = bpf_get_current_pid_tgid(), .bio = ctx[1] };
	struct frame *frame = bpf_map_lookup_elem(&frames, &key.task);
	if (!frame || !ctx[4])
		return 0;
	__u64 *epoch = bpf_map_lookup_elem(&aliases, &key);
	if (key.bio != frame->bio && (!epoch || *epoch != frame->epoch))
		return 0;
	key.bio = ctx[4];
	bpf_map_update_elem(&aliases, &key, &frame->epoch, BPF_ANY);
	return 0;
}

SEC("fentry/blk_cgroup_bio_start")
int account_operation(__u64 *ctx)
{
	struct bio *bio = (void *)ctx[0];
	struct cgroup *group = 0;
	if (BPF_CORE_READ_INTO(&group, bio, bi_blkg, blkcg, css.cgroup) || !group) {
		fail(HEALTH_READ_FAILURE);
		return 0;
	}
	struct counter_key key = {};
	__u64 *lifetime = 0;
	// Reaching the bound is an integrity failure, including for an untracked
	// deep subtree: the program cannot prove that no watched ancestor exists.
	for (int depth = 0; depth < 64; depth++) {
		if (!group)
			return 0;
		if (BPF_CORE_READ_INTO(&key.cgroup, group, kn, id)) {
			fail(HEALTH_READ_FAILURE);
			return 0;
		}
		lifetime = bpf_map_lookup_elem(&watched, &key.cgroup);
		if (lifetime)
			break;
		struct cgroup_subsys_state *parent = 0;
		if (bpf_core_read(&parent, sizeof(parent), &group->self.parent)) {
			fail(HEALTH_READ_FAILURE);
			return 0;
		}
		if (!parent)
			return 0;
		if (bpf_core_read(&group, sizeof(group), &parent->cgroup)) {
			fail(HEALTH_READ_FAILURE);
			return 0;
		}
	}
	if (!lifetime) {
		fail(HEALTH_DEPTH);
		return 0;
	}
	key.lifetime = *lifetime;
	unsigned int operation = 0;
 if (bpf_core_read(&operation, sizeof(operation), &bio->bi_opf)) {
  fail(HEALTH_READ_FAILURE);
  return 0;
 }
 // Match blk_cgroup_io_type, including flush/zeroes/zone operations.
 // Only REQ_OP_DISCARD (3) belongs to dios; op_is_write tests bit zero.
 if ((operation & 255) == 3)
  return 0;
 key.direction = operation & 1;
	struct gendisk *disk = BPF_CORE_READ(bio, bi_bdev, bd_disk);
	if (!disk || bpf_core_read(&key.major, sizeof(key.major), &disk->major) ||
	    bpf_core_read(&key.minor, sizeof(key.minor), &disk->first_minor)) {
		fail(HEALTH_READ_FAILURE);
		return 0;
	}
	struct alias_key alias = { .task = bpf_get_current_pid_tgid(), .bio = (__u64)bio };
	struct frame *frame = bpf_map_lookup_elem(&frames, &alias.task);
	if (frame) {
		__u64 *epoch = bpf_map_lookup_elem(&aliases, &alias);
		if (frame->bio == alias.bio || (epoch && *epoch == frame->epoch))
			__builtin_memcpy(key.executable, frame->executable, sizeof(key.executable));
	}
	__u64 *value = bpf_map_lookup_elem(&operations, &key);
	if (!value) {
		__u64 zero = 0;
		bpf_map_update_elem(&operations, &key, &zero, BPF_NOEXIST);
		value = bpf_map_lookup_elem(&operations, &key);
		if (!value) {
			fail(HEALTH_MAP_FULL);
			return 0;
		}
	}
	if (__sync_fetch_and_add(value, 1) == ~(__u64)0)
		fail(HEALTH_OVERFLOW);
	return 0;
}

char LICENSE[] SEC("license") = "GPL";
