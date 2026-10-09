#include <jni.h>
#include "_cgo_export.h"

JNIEXPORT jboolean JNICALL Java_github_shadowbaby_clawproxyhub_plugin_PluginService_nativeInitialize(
        JNIEnv *env, jclass type, jstring directory) {
    (void)type;
    const char *path = (*env)->GetStringUTFChars(env, directory, NULL);
    if (!path) return JNI_FALSE;
    int ok = CPHInitializePlugin((char*)path);
    (*env)->ReleaseStringUTFChars(env, directory, path);
    return ok ? JNI_TRUE : JNI_FALSE;
}

JNIEXPORT jlong JNICALL Java_github_shadowbaby_clawproxyhub_plugin_PluginService_nativeOpen(
        JNIEnv *env, jclass type, jint requests, jint callbacks) {
    (void)env; (void)type;
    return CPHOpenPlugin(requests, callbacks);
}

JNIEXPORT void JNICALL Java_github_shadowbaby_clawproxyhub_plugin_PluginService_nativeClose(
        JNIEnv *env, jclass type, jlong id) {
    (void)env; (void)type;
    CPHClosePlugin(id);
}
