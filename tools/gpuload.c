// gpuload.c - headless V3D GPU load generator via GBM render-node + EGL + GLES2.
// Binds EGL to /dev/dri/renderD128 (the v3d render node) through the GBM
// platform, then renders a heavy fragment shader into an off-screen FBO in a
// loop, glFinish()ing each frame so the kernel sees a steady stream of
// drm_sched jobs. No display/connector required.
//
//   cc gpuload.c -o gpuload -lEGL -lGLESv2 -lgbm
//   ./gpuload [seconds] [fbo_size]     (default: forever, 1024)
#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <GLES2/gl2.h>
#include <gbm.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <time.h>
#include <unistd.h>

static const char *vs =
  "attribute vec2 p; varying vec2 uv;"
  "void main(){ uv=p; gl_Position=vec4(p,0.0,1.0); }";

static const char *fs =
  "precision highp float; varying vec2 uv; uniform float t;"
  "void main(){"
  "  vec2 z=uv*3.0; float a=0.0;"
  "  for(int i=0;i<180;i++){"
  "    z=vec2(z.x*z.x-z.y*z.y, 2.0*z.x*z.y)+vec2(sin(t+uv.x),cos(t+uv.y));"
  "    a+=length(sin(z*1.3+float(i)));"
  "  }"
  "  gl_FragColor=vec4(fract(a*0.05), fract(a*0.02), fract(a*0.11), 1.0);"
  "}";

static GLuint sh(GLenum k,const char*s){
  GLuint o=glCreateShader(k); glShaderSource(o,1,&s,0); glCompileShader(o);
  GLint ok; glGetShaderiv(o,GL_COMPILE_STATUS,&ok);
  if(!ok){ char log[1024]; glGetShaderInfoLog(o,1024,0,log); fprintf(stderr,"shader: %s\n",log); exit(1);} return o;
}

int main(int argc,char**argv){
  double secs = argc>1?atof(argv[1]):0.0;   // 0 = forever
  int N = argc>2?atoi(argv[2]):1024;

  const char *node = getenv("V3D_NODE"); if(!node) node="/dev/dri/renderD128";
  int fd = open(node,O_RDWR);
  if(fd<0){ perror("open render node"); return 1; }
  struct gbm_device *gbm = gbm_create_device(fd);
  if(!gbm){ fprintf(stderr,"gbm_create_device failed\n"); return 1; }

  PFNEGLGETPLATFORMDISPLAYEXTPROC getPlatformDisplay =
    (PFNEGLGETPLATFORMDISPLAYEXTPROC)eglGetProcAddress("eglGetPlatformDisplayEXT");
  EGLDisplay dpy = getPlatformDisplay
    ? getPlatformDisplay(EGL_PLATFORM_GBM_KHR, gbm, 0)
    : eglGetDisplay((EGLNativeDisplayType)gbm);
  if(dpy==EGL_NO_DISPLAY){ fprintf(stderr,"no EGL display\n"); return 1; }
  EGLint major,minor; eglInitialize(dpy,&major,&minor);
  eglBindAPI(EGL_OPENGL_ES_API);

  // FBO-only rendering: the default-framebuffer config is irrelevant, so keep
  // the constraints minimal and just require a GLES2-renderable config.
  EGLint cfga[]={EGL_RENDERABLE_TYPE,EGL_OPENGL_ES2_BIT,EGL_SURFACE_TYPE,EGL_DONT_CARE,EGL_NONE};
  EGLConfig cfg; EGLint n=0;
  eglChooseConfig(dpy,cfga,&cfg,1,&n);
  if(n<1){ fprintf(stderr,"no EGL config (0x%x)\n",eglGetError()); return 1; }
  EGLint ctxa[]={EGL_CONTEXT_CLIENT_VERSION,2,EGL_NONE};
  EGLContext ctx=eglCreateContext(dpy,cfg,EGL_NO_CONTEXT,ctxa);
  if(ctx==EGL_NO_CONTEXT){ fprintf(stderr,"no context (0x%x)\n",eglGetError()); return 1; }
  if(!eglMakeCurrent(dpy,EGL_NO_SURFACE,EGL_NO_SURFACE,ctx)){
    fprintf(stderr,"surfaceless make-current failed (0x%x)\n",eglGetError()); return 1; }

  printf("EGL %d.%d on %s\n",major,minor,node);
  printf("GL_RENDERER: %s\n", glGetString(GL_RENDERER));
  printf("GL_VERSION : %s\n", glGetString(GL_VERSION));
  fflush(stdout);

  GLuint fbo,rb; glGenFramebuffers(1,&fbo); glBindFramebuffer(GL_FRAMEBUFFER,fbo);
  glGenRenderbuffers(1,&rb); glBindRenderbuffer(GL_RENDERBUFFER,rb);
  glRenderbufferStorage(GL_RENDERBUFFER,GL_RGBA4,N,N);
  glFramebufferRenderbuffer(GL_FRAMEBUFFER,GL_COLOR_ATTACHMENT0,GL_RENDERBUFFER,rb);
  if(glCheckFramebufferStatus(GL_FRAMEBUFFER)!=GL_FRAMEBUFFER_COMPLETE){
    fprintf(stderr,"FBO incomplete\n"); return 1; }
  glViewport(0,0,N,N);

  GLuint pr=glCreateProgram();
  glAttachShader(pr,sh(GL_VERTEX_SHADER,vs));
  glAttachShader(pr,sh(GL_FRAGMENT_SHADER,fs));
  glBindAttribLocation(pr,0,"p"); glLinkProgram(pr); glUseProgram(pr);
  GLint ut=glGetUniformLocation(pr,"t");
  float quad[]={-1,-1, 3,-1, -1,3};
  glVertexAttribPointer(0,2,GL_FLOAT,0,0,quad); glEnableVertexAttribArray(0);

  struct timespec t0; clock_gettime(CLOCK_MONOTONIC,&t0);
  unsigned long frames=0;
  for(;;){
    struct timespec now; clock_gettime(CLOCK_MONOTONIC,&now);
    double el=(now.tv_sec-t0.tv_sec)+(now.tv_nsec-t0.tv_nsec)/1e9;
    if(secs>0.0 && el>=secs) break;
    glUniform1f(ut,(float)el);
    glDrawArrays(GL_TRIANGLES,0,3);
    glFinish();
    frames++;
  }
  printf("done: %lu frames (%.1f fps)\n",frames,frames/secs>0?frames/((secs>0)?secs:1):0.0);
  return 0;
}
