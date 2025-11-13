open System.IO
open Microsoft.FSharp.Core.Result
open System
open System.Diagnostics
open System

let (>>=) f x = bind x f

let log i =
    printfn $"[LOG] {i}"
    Ok i

let getStudio () =
    let path = "./staging/MercuryStudioBeta.exe"
    // check if the file exists
    match File.Exists path with
    | true -> Ok path
    | false -> Error $"File not found: {path}"

let getArgs (id: int) (path: string) =
    // get current operating system
    let args =
        [ path
          "--script"
          $"dofile(\"http://mercs.dev/game/{id}/serve\")" ]

    Ok(
        if OperatingSystem.IsLinux() then
            [ "wine" ] @ args
        else
            args
    )

let startGameserver (args: string list) =
    try
        let startInfo = ProcessStartInfo()
        startInfo.FileName <- args.Head
        startInfo.Arguments <- String.Join(" ", args.Tail)
        startInfo.UseShellExecute <- false
        startInfo.RedirectStandardOutput <- true
        startInfo.RedirectStandardError <- true

        let proc = Process.Start startInfo
        proc.EnableRaisingEvents <- true

        proc.OutputDataReceived.Add(fun data -> printfn $"[STDOUT] {data.Data}")
        // proc.ErrorDataReceived.Add(fun data -> printfn $"[STDERR] {data.Data}")

        proc.BeginOutputReadLine()
        proc.BeginErrorReadLine()

        Ok proc
    with
    | ex -> Error $"Failed to start gameserver: {ex.Message}"

let pollMonitor (getter: unit -> 'a) (callback: 'a -> unit) =
    let rec loop () =
        async {
            callback (getter ())
            do! Async.Sleep 1000
            return! loop ()
        }

    Async.Start(loop ())

let monitorGameserver (proc: Process) =
    proc.Exited.Add(fun _ -> printfn "[INFO] Gameserver process has exited.")

    // get when the process is responding
    pollMonitor
        (fun () -> proc.Responding)
        (fun responding ->
            if responding then
                printfn "[INFO] Gameserver process is responding."
            else
                printfn "[WARN] Gameserver process is not responding."

            let success = proc.CloseMainWindow()

            if not success then
                printfn "[WARN] Failed to close main window of gameserver process.")


    |> ignore

    // print whenever window title changes
    proc.WaitForExitAsync()
    |> Async.AwaitTask
    |> Async.RunSynchronously

    Ok()


let newGameserver (id: int) =
    getStudio ()
    >>= log
    >>= getArgs id
    >>= log
    >>= startGameserver
    >>= log
    >>= monitorGameserver

let start (id: int) =
    printfn $"Received start request for ID {id}"

    newGameserver id

// wait for all async tasks to complete

[<EntryPoint>]
let main _ =
    match start 42 with
    | Ok _ -> 0
    | Error msg ->
        printfn $"Error: {msg}"
        1
