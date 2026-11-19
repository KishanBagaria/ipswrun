// Foundation-heavy guest: collections, JSON, property lists, dates, regular expressions, files.
#import <Foundation/Foundation.h>

int main(int argc, char **argv) {
    @autoreleasepool {
        NSOperatingSystemVersion v = NSProcessInfo.processInfo.operatingSystemVersion;
        printf("os %ld.%ld.%ld\n", (long)v.majorVersion, (long)v.minorVersion, (long)v.patchVersion);
        printf("args %d home %s\n", argc, NSHomeDirectory().UTF8String);

        NSMutableDictionary *d = [NSMutableDictionary dictionary];
        for (int i = 0; i < 1000; i++) d[[NSString stringWithFormat:@"key%d", i]] = @(i * i);
        NSArray *keys = [d.allKeys sortedArrayUsingSelector:@selector(localizedStandardCompare:)];
        printf("dict %lu last %s\n", (unsigned long)d.count, [keys.lastObject UTF8String]);

        NSError *error = nil;
        NSData *json = [NSJSONSerialization dataWithJSONObject:@{@"a": @[@1, @2.5, @"three", NSNull.null], @"b": @{@"c": @YES}}
                                                       options:NSJSONWritingSortedKeys error:&error];
        printf("json %s\n", [[NSString alloc] initWithData:json encoding:NSUTF8StringEncoding].UTF8String);
        id back = [NSJSONSerialization JSONObjectWithData:json options:0 error:&error];
        printf("json round trip %d\n", [back[@"b"][@"c"] boolValue]);

        NSData *plist = [NSPropertyListSerialization dataWithPropertyList:@{@"date": [NSDate dateWithTimeIntervalSince1970:86400], @"data": json}
                                                                   format:NSPropertyListBinaryFormat_v1_0 options:0 error:&error];
        id plistBack = [NSPropertyListSerialization propertyListWithData:plist options:0 format:NULL error:&error];
        printf("plist %lu bytes, date %.0f\n", (unsigned long)plist.length, [plistBack[@"date"] timeIntervalSince1970]);

        NSDateFormatter *f = [NSDateFormatter new];
        f.locale = [NSLocale localeWithLocaleIdentifier:@"en_US_POSIX"];
        f.timeZone = [NSTimeZone timeZoneForSecondsFromGMT:0];
        f.dateFormat = @"yyyy-MM-dd'T'HH:mm:ss'Z'";
        printf("date %s\n", [f stringFromDate:[NSDate dateWithTimeIntervalSince1970:1700000000]].UTF8String);
        NSDateComponents *c = [[NSCalendar calendarWithIdentifier:NSCalendarIdentifierGregorian]
            components:NSCalendarUnitYear | NSCalendarUnitWeekday fromDate:[NSDate dateWithTimeIntervalSince1970:0]];
        printf("calendar year %ld weekday %ld\n", (long)c.year, (long)c.weekday);

        NSRegularExpression *re = [NSRegularExpression regularExpressionWithPattern:@"(\\w+)@(\\w+)\\.com" options:0 error:&error];
        NSString *text = @"mail alice@example.com and bob@apple.com";
        printf("regex %lu matches\n", (unsigned long)[re numberOfMatchesInString:text options:0 range:NSMakeRange(0, text.length)]);

        NSNumberFormatter *nf = [NSNumberFormatter new];
        nf.locale = [NSLocale localeWithLocaleIdentifier:@"de_DE"];
        nf.numberStyle = NSNumberFormatterDecimalStyle;
        printf("number %s\n", [nf stringFromNumber:@1234567.891].UTF8String);

        NSURLComponents *u = [NSURLComponents componentsWithString:@"https://user@example.com:8443/a%20b/c?q=1&r=two#frag"];
        printf("url host %s port %s path %s items %lu\n", u.host.UTF8String, u.port.stringValue.UTF8String, u.path.UTF8String,
               (unsigned long)u.queryItems.count);

        NSString *b64 = [[@"héllo wörld" dataUsingEncoding:NSUTF8StringEncoding] base64EncodedStringWithOptions:0];
        printf("base64 %s upper %s\n", b64.UTF8String, [@"straße" uppercaseString].UTF8String);

        NSFileManager *fm = NSFileManager.defaultManager;
        NSString *dir = [NSHomeDirectory() stringByAppendingPathComponent:@"out/nested"];
        BOOL ok = [fm createDirectoryAtPath:dir withIntermediateDirectories:YES attributes:nil error:&error];
        NSString *path = [dir stringByAppendingPathComponent:@"note.txt"];
        ok = ok && [@"written by the guest\n" writeToFile:path atomically:YES encoding:NSUTF8StringEncoding error:&error];
        ok = ok && [json writeToFile:[dir stringByAppendingPathComponent:@"a.json"] atomically:NO];
        NSString *readBack = [NSString stringWithContentsOfFile:path encoding:NSUTF8StringEncoding error:&error];
        NSArray *listing = [[fm contentsOfDirectoryAtPath:dir error:&error] sortedArrayUsingSelector:@selector(compare:)];
        NSDictionary *attrs = [fm attributesOfItemAtPath:path error:&error];
        printf("files ok %d read %lu listing %s size %llu\n", ok, (unsigned long)readBack.length,
               [listing componentsJoinedByString:@","].UTF8String, [attrs fileSize]);
        if (error) printf("error %s\n", error.description.UTF8String);

        NSDictionary *sv = [NSDictionary dictionaryWithContentsOfFile:@"/System/Library/CoreServices/SystemVersion.plist"];
        printf("build %s\n", [sv[@"ProductBuildVersion"] UTF8String]);
        NSUUID *uuid = [NSUUID UUID];
        printf("uuid length %lu\n", (unsigned long)uuid.UUIDString.length);
    }
    return 0;
}
