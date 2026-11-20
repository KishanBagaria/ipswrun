// Scratch guest: memory mapped twice is one memory.
#include <mach/mach.h>
#include <mach/mach_vm.h>
#include <stdio.h>
#include <string.h>

int main(void) {
    mach_port_t task = mach_task_self();
    mach_vm_address_t a = 0, b = 0, c = 0, d = 0;
    mach_vm_size_t size = 3 * vm_page_size;
    mach_vm_allocate(task, &a, size, VM_FLAGS_ANYWHERE);
    strcpy((char *)a + 100, "before");

    memory_object_size_t entry_size = size;
    mach_port_t entry = MACH_PORT_NULL;
    kern_return_t kr = mach_make_memory_entry_64(task, &entry_size, a, VM_PROT_READ | VM_PROT_WRITE, &entry, MACH_PORT_NULL);
    printf("entry: %d, size %llu pages\n", kr, (unsigned long long)(entry_size / vm_page_size));
    kr = mach_vm_map(task, &b, size, 0, VM_FLAGS_ANYWHERE, entry, 0, FALSE, VM_PROT_READ | VM_PROT_WRITE, VM_PROT_READ | VM_PROT_WRITE, VM_INHERIT_NONE);
    printf("map shared: %d, sees \"%s\"\n", kr, kr ? "" : (char *)b + 100);
    strcpy((char *)b + 100, "through b");
    strcpy((char *)a + 2 * vm_page_size, "through a");
    printf("a sees \"%s\", b sees \"%s\"\n", (char *)a + 100, (char *)b + 2 * vm_page_size);

    kr = mach_vm_map(task, &c, size, 0, VM_FLAGS_ANYWHERE, entry, 0, TRUE, VM_PROT_READ | VM_PROT_WRITE, VM_PROT_READ | VM_PROT_WRITE, VM_INHERIT_NONE);
    strcpy((char *)a + 100, "after the copy");
    printf("map copy: %d, keeps \"%s\"\n", kr, kr ? "" : (char *)c + 100);

    vm_prot_t cur, max;
    kr = mach_vm_remap(task, &d, size, 0, VM_FLAGS_ANYWHERE, task, a, FALSE, &cur, &max, VM_INHERIT_NONE);
    strcpy((char *)d + 100, "through the remap");
    printf("remap: %d, a sees \"%s\", b sees \"%s\"\n", kr, (char *)a + 100, (char *)b + 100);
    return 0;
}
