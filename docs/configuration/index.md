# 설정

imgsync 설정은 세 영역으로 나뉜다.

1. **환경 변수** — 런타임에서 컨테이너에 주입하는 값 (DB 접속, 로그 레벨 등)
2. **Helm values** — 클러스터 배포 시점의 토폴로지 / replica / resource 설정
3. **컴포넌트별 Config 구조** — 코드 인터페이스로 노출되는 Worker / Sniffer / Sweeper / Protocol 설정

| 영역 | 페이지 |
|---|---|
| 런타임 | [환경 변수](environment-variables.md) |
| Worker | [Worker 설정](worker.md) |
| Sniffer | [Sniffer 설정](sniffer.md) |
| Sweeper | [Sweeper 설정](sweeper.md) |
| 입력/출력 | [프로토콜](protocols.md) |

배포 시점 설정은 [values.yaml 레퍼런스](../installation/values-reference.md) 참고.
