// Only count HTTP errors entering the model classifier of the pinned amd64 CLI.
// Turn and diagnostic attribution are verified separately against the worker's
// own session database. Ordinary output and prompt processing need no hooks.
const counts = Memory.alloc(200 * 4);
const native = new CModule(`
#include <gum/guminterceptor.h>
extern guint32 counts[200];

void classified(GumInvocationContext *ic) {
    const guint8 *error = (const guint8 *)ic->cpu_context->rax;
    if (error == NULL) return;
    const gint code = *(const gint *)(error + 0x64);
    if (code >= 400 && code <= 599) {
        __asm__ volatile ("lock incl %0" : "+m" (counts[code - 400]) : : "cc", "memory");
    }
}
`, { counts });
Interceptor.attach(Process.mainModule.base.add(0xa6be573), native.classified);
rpc.exports = {
    snapshot() { return counts.readByteArray(200 * 4); }
};
