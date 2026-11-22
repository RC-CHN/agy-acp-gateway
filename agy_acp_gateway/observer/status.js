// Linux/amd64 agy 1.5.14, build 37873b80. Instruction callbacks execute on a
// Go stack. Keep them native, bounded and independent of the JS runtime.
const base = Process.mainModule.base;
const libc = Process.getModuleByName('libc.so.6');
const moduleState = Memory.alloc(8192);
const native = new CModule(`
#include <gum/guminterceptor.h>
#include <string.h>

typedef guint64 Word;
typedef struct { Word object, data, length; gint code; gboolean model; } Source;
typedef struct {
    Word lock[8];
    Source sources[64];
    guint count, messages;
    gboolean active;
    gint64 last_step, error_step;
    gint last_kind, code;
    Word text_object, bytes;
    guint error_message;
    char sid[36];
} State;

extern State state;
extern char image_base[];
extern int compare_id(const void *,const void *,gsize);
extern int lock_state(void *);
extern int unlock_state(void *);

static Word word(Word base, Word offset) { return *(Word *)(base + offset); }
static gint integer(Word base, Word offset) { return *(gint *)(base + offset); }
static GumCpuContext *registers(GumInvocationContext * ic) {
    return ic->cpu_context;
}

// Per-session worker starts another prompt. All prior receipts expire here.
void begin(GumInvocationContext *ic) {
    lock_state(state.lock);
    state.count = state.messages = state.code = state.error_message = 0;
    state.text_object = 0;
    state.last_step = -1;
    state.active = TRUE;
    unlock_state(state.lock);
}

// HTTPError.ToCortexErrorDetails after it assigns ErrorCode and Details.
// Only read the backing address and length of Details, never its contents.
void converted(GumInvocationContext *ic) {
    Word p = registers(ic)->rax;
    lock_state(state.lock);
    if (state.active && state.count < 64) {
        Source *s = &state.sources[state.count++];
        s->object = p;
        s->code = integer(p, 0x64);
        s->data = word(p, 0x68);
        s->length = word(p, 0x70);
        s->model = FALSE;
    } else { state.active = FALSE; }
    unlock_state(state.lock);
}

static gboolean same(Source *s, Word p) {
    return s->data && s->length && s->code >= 400 && s->code <= 599 &&
           s->code == integer(p, 0x64) && s->data == word(p, 0x68) &&
           s->length == word(p, 0x70);
}

// modelapiccpa.ClassifyError just received the converted object.
void classified(GumInvocationContext *ic) {
    Word p = registers(ic)->rax;
    lock_state(state.lock);
    for (guint i=0; state.active && p && i<state.count; i++) {
        Source *s = &state.sources[i];
        if (s->object == p && same(s,p)) s->model = TRUE;
    }
    unlock_state(state.lock);
}

// The active-turn session check has passed. Track the final trajectory step.
void step(GumInvocationContext *ic) {
    Word sp = registers(ic)->rsp;
    gint64 index = word(sp, 0x78);
    Word p = word(sp, 0x80);
    lock_state(state.lock);
    if (p && index >= state.last_step) {
        state.last_step = index;
        state.last_kind = integer(p, 8);
    }
    unlock_state(state.lock);
}

// Error-only PendingText delta just boxed by convTstring. Match its Go string
// header object with the one passed to the ACP agent_message_chunk serializer.
void diagnostic(GumInvocationContext *ic) {
    GumCpuContext *r = registers(ic);
    if (word(r->rbp,8) != (Word)image_base + 0xac1008f) return;
    Word sp = r->rsp, p = word(sp,0x200);
    if (!p || word(sp,0x4c8) != 36) return;
    lock_state(state.lock);
    state.code = state.error_message = 0;
    state.text_object = 0;
    for (guint i=0; state.active && i<state.count; i++) {
        Source *s = &state.sources[i];
        if (!s->model || !same(s,p)) continue;
        state.code = s->code;
        state.text_object = r->rax;
        state.bytes = word(r->rax, 8);
        state.error_step = word(sp,0x4d0);
        memcpy(state.sid,(void *)word(sp,0x4c0),36);
        break;
    }
    unlock_state(state.lock);
}

void message(GumInvocationContext *ic) {
    GumCpuContext *r = registers(ic);
    lock_state(state.lock);
    state.messages++;
    if (state.text_object && r->rax == (Word)image_base + 0xd254f88 &&
        r->rbx == state.text_object) state.error_message = state.messages;
    state.text_object = 0;
    unlock_state(state.lock);
}

int match(const char *sid, guint count, guint bytes) {
    int code = 0;
    lock_state(state.lock);
    if (state.active && state.code && count && count == state.messages &&
        count == state.error_message && state.bytes == bytes && state.last_kind == 17 &&
        state.last_step == state.error_step && compare_id(state.sid,sid,36) == 0) code = state.code;
    unlock_state(state.lock);
    return code;
}
`, {
    state: moduleState, image_base: base,
    compare_id: libc.getExportByName('memcmp'),
    lock_state: libc.getExportByName('pthread_mutex_lock'),
    unlock_state: libc.getExportByName('pthread_mutex_unlock')
});
for (const [offset, fn] of [
    [0xac00707, native.begin], [0x894a094, native.converted],
    [0xa6be573, native.classified], [0xac10061, native.step],
    [0xac1137d, native.diagnostic], [0x75739ae, native.message]
]) Interceptor.attach(base.add(offset), fn);
const match = new NativeFunction(native.match, 'int', ['pointer', 'uint', 'uint']);
rpc.exports = {
    ready() { return true; },
    match(sid, count, bytes) {
        if (!/^[0-9a-f-]{36}$/.test(sid)) return 0;
        return match(Memory.allocUtf8String(sid), count, bytes);
    }
};
