#include <jni.h>
#include <stdlib.h>
#include <stdint.h>

/*
 * Go c-shared exports from libnpc.so
 *
 * IMPORTANT: GoInt = long long (int64_t) on 64-bit platforms.
 * The doc's snippet used `int` which is WRONG for arm64.
 * Using int64_t to match the actual Go c-shared ABI.
 */
extern int64_t StartClientByVerifyKey(char* serverAddr, char* verifyKey, char* connType, char* proxyUrl);
extern int64_t GetClientStatus(void);
extern void    CloseClient(void);
extern char*   Version(void);
extern char*   Logs(void);
extern void    SetLogsLevel(char* level);
extern void    SetDnsServer(char* dns);

/* ---- JNI methods for class com.npc.app.NpcNative ---- */

JNIEXPORT jint JNICALL
Java_com_npc_app_NpcNative_startClient(JNIEnv *env, jclass clazz,
                                       jstring jServer, jstring jVkey,
                                       jstring jType, jstring jProxy) {
    const char *s = (*env)->GetStringUTFChars(env, jServer, 0);
    const char *k = (*env)->GetStringUTFChars(env, jVkey, 0);
    const char *t = (*env)->GetStringUTFChars(env, jType, 0);
    const char *p = jProxy ? (*env)->GetStringUTFChars(env, jProxy, 0) : "";

    jint ret = (jint) StartClientByVerifyKey((char*)s, (char*)k, (char*)t, (char*)p);

    (*env)->ReleaseStringUTFChars(env, jServer, s);
    (*env)->ReleaseStringUTFChars(env, jVkey, k);
    (*env)->ReleaseStringUTFChars(env, jType, t);
    if (jProxy) (*env)->ReleaseStringUTFChars(env, jProxy, p);
    return ret;
}

JNIEXPORT jint JNICALL
Java_com_npc_app_NpcNative_getStatus(JNIEnv *env, jclass clazz) {
    return (jint) GetClientStatus();
}

JNIEXPORT void JNICALL
Java_com_npc_app_NpcNative_closeClient(JNIEnv *env, jclass clazz) {
    CloseClient();
}

JNIEXPORT jstring JNICALL
Java_com_npc_app_NpcNative_version(JNIEnv *env, jclass clazz) {
    char *v = Version();
    return (*env)->NewStringUTF(env, v ? v : "unknown");
}

JNIEXPORT jstring JNICALL
Java_com_npc_app_NpcNative_logs(JNIEnv *env, jclass clazz) {
    char *l = Logs();
    return (*env)->NewStringUTF(env, l ? l : "");
}

JNIEXPORT void JNICALL
Java_com_npc_app_NpcNative_setLogsLevel(JNIEnv *env, jclass clazz, jstring jLevel) {
    const char *lv = (*env)->GetStringUTFChars(env, jLevel, 0);
    SetLogsLevel((char*)lv);
    (*env)->ReleaseStringUTFChars(env, jLevel, lv);
}

JNIEXPORT void JNICALL
Java_com_npc_app_NpcNative_setDnsServer(JNIEnv *env, jclass clazz, jstring jDns) {
    const char *d = (*env)->GetStringUTFChars(env, jDns, 0);
    SetDnsServer((char*)d);
    (*env)->ReleaseStringUTFChars(env, jDns, d);
}
