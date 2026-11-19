// Objective-C introspection guest: walks the runtime's view of the shared cache and of
// frameworks it loads by path (arguments), which is how a class dump of any build starts.
#import <Foundation/Foundation.h>
#import <objc/runtime.h>
#import <objc/message.h>
#import <dlfcn.h>
#import <mach-o/dyld.h>

static void describe(const char *name) {
    Class c = objc_getClass(name);
    if (!c) { printf("%s: missing\n", name); return; }
    unsigned methods = 0, classMethods = 0, ivars = 0, props = 0, protos = 0;
    free(class_copyMethodList(c, &methods));
    free(class_copyMethodList(object_getClass(c), &classMethods));
    free(class_copyIvarList(c, &ivars));
    free(class_copyPropertyList(c, &props));
    free(class_copyProtocolList(c, &protos));
    int depth = 0;
    for (Class s = c; s; s = class_getSuperclass(s)) depth++;
    printf("%s: %u methods, %u class methods, %u ivars, %u properties, %u protocols, depth %d, size %zu, image %s\n", name, methods,
           classMethods, ivars, props, protos, depth, class_getInstanceSize(c), strrchr(class_getImageName(c) ?: "/?", '/') + 1);
}

@interface Probe : NSObject
@property(nonatomic, copy) NSString *label;
@end
@implementation Probe
- (NSString *)greet:(NSString *)who { return [NSString stringWithFormat:@"hello %@ from %@", who, self.label]; }
@end

int main(int argc, char **argv) {
    @autoreleasepool {
        unsigned before = 0;
        free(objc_copyClassList(&before));
        printf("classes at start %u, images %u\n", before, _dyld_image_count());
        describe("NSObject"); describe("NSString"); describe("NSMutableDictionary"); describe("NSURLSession");

        for (int i = 1; i < argc; i++) {
            void *h = dlopen(argv[i], RTLD_NOW);
            // The runtime knows an image by the path dyld loaded it from, which need not be argv[i].
            const char *base = strrchr(argv[i], '/') ? strrchr(argv[i], '/') : argv[i], *image = argv[i];
            for (uint32_t k = _dyld_image_count(); h && k-- > 0;) {
                const char *name = _dyld_get_image_name(k), *slash = strrchr(name, '/');
                if (slash && !strcmp(slash, base)) { image = name; break; }
            }
            unsigned n = 0;
            const char **names = h ? objc_copyClassNamesForImage(image, &n) : NULL;
            printf("%s: %s, %u classes", strrchr(argv[i], '/') + 1, h ? "loaded" : dlerror(), n);
            for (unsigned j = 0; j < n && j < 3; j++) printf("%s%s", j ? ", " : ": ", names[j]);
            printf("\n");
            for (unsigned j = 0; j < n && j < 2; j++) describe(names[j]);
            free(names);
        }
        unsigned after = 0;
        free(objc_copyClassList(&after));
        printf("classes now %u, images %u\n", after, _dyld_image_count());

        Method m = class_getInstanceMethod(NSString.class, @selector(stringByAppendingString:));
        printf("encoding %s\n", method_getTypeEncoding(m));
        NSMethodSignature *sig = [NSString instanceMethodSignatureForSelector:@selector(rangeOfString:options:)];
        printf("signature %lu args, returns %s\n", (unsigned long)sig.numberOfArguments, sig.methodReturnType);

        // Build a class at run time, swizzle a method and call through NSInvocation and KVC.
        Class dyn = objc_allocateClassPair(Probe.class, "DynamicProbe", 0);
        class_addMethod(dyn, @selector(shout), imp_implementationWithBlock(^(id self) { return [[self label] uppercaseString]; }), "@@:");
        objc_registerClassPair(dyn);
        Probe *p = [dyn new];
        [p setValue:@"runtime" forKey:@"label"];
        printf("dynamic %s %s\n", [((id (*)(id, SEL))objc_msgSend)(p, @selector(shout)) UTF8String], class_getName(object_getClass(p)));
        NSInvocation *inv = [NSInvocation invocationWithMethodSignature:[p methodSignatureForSelector:@selector(greet:)]];
        inv.selector = @selector(greet:);
        NSString *who = @"invocation";
        [inv setArgument:&who atIndex:2];
        [inv invokeWithTarget:p];
        __unsafe_unretained NSString *out = nil;
        [inv getReturnValue:&out];
        printf("%s\n", out.UTF8String);
        Protocol *proto = objc_getProtocol("NSSecureCoding");
        printf("NSSecureCoding %d, NSString conforms %d\n", proto != nil, [NSString conformsToProtocol:proto]);
    }
    return 0;
}
