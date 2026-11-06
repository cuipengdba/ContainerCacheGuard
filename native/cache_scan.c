/* SPDX-License-Identifier: MIT
 * Read-only Linux mincore inventory. Does not fault/read file data into RAM.
 * Traversal does touch inode/dentry metadata; measure that cost separately.
 */
#define _XOPEN_SOURCE 700
#define _DEFAULT_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <ftw.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <time.h>
#include <unistd.h>
static uint64_t files,bytes,pages,resident,resident_bytes,errors;
static long page_size;
static int visit(const char *path,const struct stat *st,int flag,struct FTW *f)
{
    (void)f;
    if(flag==FTW_DNR||flag==FTW_NS){errors++;return 0;}
    if(flag!=FTW_F||!S_ISREG(st->st_mode))return 0;
    files++;bytes+=(uint64_t)st->st_size;
    if(!st->st_size)return 0;
    size_t n=((size_t)st->st_size+(size_t)page_size-1)/(size_t)page_size;
    int fd=open(path,O_RDONLY|O_CLOEXEC|O_NOFOLLOW);
    if(fd<0){errors++;return 0;}
    void *mapping=mmap(NULL,(size_t)st->st_size,PROT_NONE,MAP_SHARED,fd,0);
    unsigned char *vec=malloc(n);
    if(mapping==MAP_FAILED||!vec){errors++;if(mapping!=MAP_FAILED)munmap(mapping,(size_t)st->st_size);free(vec);close(fd);return 0;}
    if(mincore(mapping,(size_t)st->st_size,vec)){errors++;}
    else{
        pages+=n;
        for(size_t i=0;i<n;i++)if(vec[i]&1){
            resident++;
            uint64_t remaining=(uint64_t)st->st_size-i*(uint64_t)page_size;
            resident_bytes+=remaining<(uint64_t)page_size?remaining:(uint64_t)page_size;
        }
    }
    free(vec);munmap(mapping,(size_t)st->st_size);close(fd);return 0;
}
int main(int argc,char **argv)
{
    if(argc!=2){fprintf(stderr,"usage: cache-scan /absolute/directory\n");return 2;}
    if(argv[1][0]!='/'){fprintf(stderr,"absolute directory required\n");return 2;}
    page_size=sysconf(_SC_PAGESIZE);
    struct timespec start,end;clock_gettime(CLOCK_MONOTONIC,&start);
    if(nftw(argv[1],visit,32,FTW_PHYS|FTW_MOUNT)){perror("nftw");return 1;}
    clock_gettime(CLOCK_MONOTONIC,&end);
    double seconds=(double)(end.tv_sec-start.tv_sec)+(double)(end.tv_nsec-start.tv_nsec)/1e9;
    printf("{\"files\":%llu,\"file_bytes\":%llu,\"pages\":%llu,\"resident_pages\":%llu,\"resident_bytes\":%llu,\"errors\":%llu,\"scan_seconds\":%.6f,\"method\":\"mincore_no_data_faults\"}\n",
           (unsigned long long)files,(unsigned long long)bytes,(unsigned long long)pages,
           (unsigned long long)resident,(unsigned long long)resident_bytes,(unsigned long long)errors,seconds);
    return errors?1:0;
}
