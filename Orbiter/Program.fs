open System.IO
open Microsoft.FSharp.Core.Result
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
    let start =
        if OperatingSystem.IsLinux() then
            [ "wine" ]
        else
            []

    let args =
        start
        @ [ path
            "--script"
            $"dofile(\"http://mercs.dev/game/{id}/serve\")" ]

    Ok args

let newGameserver (id: int) =
    getStudio ()
    >>= log
    >>= getArgs id
    >>= log
    >>= fun path ->
            printfn $"Starting gameserver with ID {id} using studio at {path}"
            Ok()


let start (id: int) =
    printfn $"Received start request for ID {id}"

    newGameserver id

[<EntryPoint>]
let main _ =
    match start 42 with
    | Ok _ -> 0
    | Error msg ->
        printfn $"Error: {msg}"
        1
